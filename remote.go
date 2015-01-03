// Copyright 2013 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drive

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"code.google.com/p/goauth2/oauth"
	drive "github.com/google/google-api-go-client/drive/v2"
	"github.com/neilcawse/drive/config"
)

const (
	// Google OAuth 2.0 service URLs
	GoogleOAuth2AuthURL  = "https://accounts.google.com/o/oauth2/auth"
	GoogleOAuth2TokenURL = "https://accounts.google.com/o/oauth2/token"

	// OAuth 2.0 OOB redirect URL for authorization.
	RedirectURL = "urn:ietf:wg:oauth:2.0:oob"

	// OAuth 2.0 full Drive scope used for authorization.
	DriveScope = "https://www.googleapis.com/auth/drive"

	// OAuth 2.0 access type for offline/refresh access.
	AccessType = "offline"
)

var (
	ErrPathNotExists = errors.New("remote path doesn't exist")
)

var (
	UnescapedPathSep = fmt.Sprintf("%c", os.PathSeparator)
	EscapedPathSep   = url.QueryEscape(UnescapedPathSep)
)

var regExtStrMap = map[string]string{
	"csv":   "text/csv",
	"html?": "text/html",
	"te?xt": "text/plain",

	"gif":   "image/gif",
	"png":   "image/png",
	"svg":   "image/svg+xml",
	"jpe?g": "image/jpeg",

	"odt": "application/vnd.oasis.opendocument.text",
	"rtf": "application/rtf",
	"pdf": "application/pdf",

	"docx?": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"pptx?": "application/vnd.openxmlformats-officedocument.wordprocessingml.presentation",
	"xlsx?": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

func compileRegExtMap() *map[*regexp.Regexp]string {
	regExpMap := make(map[*regexp.Regexp]string)
	for regStr, mimeType := range regExtStrMap {
		regExComp, err := regexp.Compile(regStr)
		if err == nil {
			regExpMap[regExComp] = mimeType
		}
	}
	return &regExpMap
}

var regExtMap = *compileRegExtMap()

func mimeTypeFromExt(ext string) string {
	bExt := []byte(ext)
	for regEx, mimeType := range regExtMap {
		if regEx != nil && regEx.Match(bExt) {
			return mimeType
		}
	}
	return ""
}

type Remote struct {
	transport *oauth.Transport
	service   *drive.Service
}

func NewRemoteContext(context *config.Context) *Remote {
	transport := newTransport(context)
	service, _ := drive.New(transport.Client())
	return &Remote{service: service, transport: transport}
}

func hasExportLinks(f *File) bool {
	if f == nil || f.IsDir {
		return false
	}
	return len(f.ExportLinks) >= 1
}

func RetrieveRefreshToken(context *config.Context) (string, error) {
	transport := newTransport(context)
	url := transport.Config.AuthCodeURL("")
	fmt.Println("Visit this URL to get an authorization code")
	fmt.Println(url)
	fmt.Print("Paste the authorization code: ")
	var code string
	fmt.Scanln(&code)
	token, err := transport.Exchange(code)
	if err != nil {
		return "", err
	}
	return token.RefreshToken, nil
}

func (r *Remote) FindById(id string) (file *File, err error) {
	req := r.service.Files.Get(id)
	var f *drive.File
	if f, err = req.Do(); err != nil {
		return
	}
	return NewRemoteFile(f), nil
}

func (r *Remote) FindByPath(p string) (file *File, err error) {
	if p == "/" {
		return r.FindById("root")
	}
	parts := strings.Split(p, "/") // TODO: use path.Split instead
	return r.findByPathRecv("root", parts[1:])
}

func (r *Remote) FindByPathTrashed(p string) (file *File, err error) {
	if p == "/" {
		return r.FindById("root")
	}
	parts := strings.Split(p, "/") // TODO: use path.Split instead
	return r.findByPathTrashed("root", parts[1:])
}

func (r *Remote) findByParentIdRaw(parentId string, trashed bool) ([]*File, error) {
	pageToken := ""
	var files []*File
	for {
	    req := r.service.Files.List()

	    // TODO: use field selectors
	    var expr string
	    if trashed {
		    expr = fmt.Sprintf("'%s' in parents and trashed=true", parentId)
	    } else {
		    expr = fmt.Sprintf("'%s' in parents and trashed=false", parentId)
	    }

	    req.Q(expr)
	    if pageToken != "" {
			req.PageToken(pageToken)
		}
	    results, err := req.Do()
	    if err != nil {
	        return files, err
	    }
	    for _, f := range results.Items {
		    if !strings.HasPrefix(f.Title, ".") { // ignore hidden files
			    files = append(files, NewRemoteFile(f))
		    }
	    }
	    pageToken = results.NextPageToken
	    if pageToken == "" {
		    return files, err
	    }
    }
}

func (r *Remote) FindByParentId(parentId string) (files []*File, err error) {
	return r.findByParentIdRaw(parentId, false)
}

func (r *Remote) FindByParentIdTrashed(parentId string) (files []*File, err error) {
	return r.findByParentIdRaw(parentId, true)
}

func (r *Remote) Trash(id string) error {
	_, err := r.service.Files.Trash(id).Do()
	return err
}

func (r *Remote) Untrash(id string) error {
	_, err := r.service.Files.Untrash(id).Do()
	return err
}

func (r *Remote) Unpublish(id string) error {
	return r.service.Permissions.Delete(id, "anyone").Do()
}

func (r *Remote) Publish(id string) (string, error) {
	perm := &drive.Permission{Type: "anyone", Role: "reader"}
	_, err := r.service.Permissions.Insert(id, perm).Do()
	if err != nil {
		return "", err
	}
	return "https://googledrive.com/host/" + id, nil
}

func urlToPath(p string, fsBound bool) string {
	if fsBound {
		return strings.Replace(p, UnescapedPathSep, EscapedPathSep, -1)
	}
	return strings.Replace(p, EscapedPathSep, UnescapedPathSep, -1)
}

func (r *Remote) Download(id string, exportURL string) (io.ReadCloser, error) {
	var url string
	if len(exportURL) < 1 {
		url = "https://googledrive.com/host/" + id
	} else {
		url = exportURL
	}
	resp, err := r.transport.Client().Get(url)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Body, err
	}
	return resp.Body, nil
}

func (r *Remote) Upsert(parentId string, file *File, body io.Reader) (f *File, err error) {
	uploaded := &drive.File{
		Title:   file.Name,
		Parents: []*drive.ParentReference{&drive.ParentReference{Id: parentId}},
	}
	if file.IsDir {
		uploaded.MimeType = "application/vnd.google-apps.folder"
	}

	if file.Id == "" {
		req := r.service.Files.Insert(uploaded)
		if !file.IsDir && body != nil {
			req = req.Media(body)
		}
		if uploaded, err = req.Do(); err != nil {
			return
		}
		return NewRemoteFile(uploaded), nil
	}
	// update the existing
	req := r.service.Files.Update(file.Id, uploaded)
	if !file.IsDir && body != nil {
		req = req.Media(body)
	}
	if uploaded, err = req.Do(); err != nil {
		return
	}
	return NewRemoteFile(uploaded), nil
}

func (r *Remote) findByPathRecvRaw(parentId string, p []string, trashed bool) (file *File, err error) {
	// find the file or directory under parentId and titled with p[0]
	req := r.service.Files.List()
	// TODO: use field selectors
	var expr string
	head := urlToPath(p[0], false)
	if trashed {
		expr = fmt.Sprintf("title = '%s' and trashed=true", head)
	} else {
		expr = fmt.Sprintf("'%s' in parents and title = '%s' and trashed=false", parentId, head)
	}
	req.Q(expr)
	files, err := req.Do()
	if err != nil || len(files.Items) < 1 {
		// TODO: make sure only 404s are handled here
		return nil, ErrPathNotExists
	}
	file = NewRemoteFile(files.Items[0])
	if len(p) == 1 {
		return file, nil
	}
	return r.findByPathRecvRaw(file.Id, p[1:], trashed)
}

func (r *Remote) findByPathRecv(parentId string, p []string) (file *File, err error) {
	return r.findByPathRecvRaw(parentId, p, false)
}

func (r *Remote) findByPathTrashed(parentId string, p []string) (file *File, err error) {
	return r.findByPathRecvRaw(parentId, p, true)
}

func newAuthConfig(context *config.Context) *oauth.Config {
	return &oauth.Config{
		ClientId:     context.ClientId,
		ClientSecret: context.ClientSecret,
		AuthURL:      GoogleOAuth2AuthURL,
		TokenURL:     GoogleOAuth2TokenURL,
		RedirectURL:  RedirectURL,
		AccessType:   AccessType,
		Scope:        DriveScope,
	}
}

func newTransport(context *config.Context) *oauth.Transport {
	return &oauth.Transport{
		Config:    newAuthConfig(context),
		Transport: http.DefaultTransport,
		Token: &oauth.Token{
			RefreshToken: context.RefreshToken,
			Expiry:       time.Now(),
		},
	}
}
