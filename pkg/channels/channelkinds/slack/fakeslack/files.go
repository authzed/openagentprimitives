package fakeslack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	slackapi "github.com/slack-go/slack"
)

// fileFixture is one seeded inbound attachment's bytes, servable at the
// private-download URL FetchAttachment's second leg hits — a raw HTTP GET
// against file.URLPrivateDownload (pkg/channels/channelkinds/slack/attachments.go),
// not a Web API method, so it cannot be modeled as a plain Go method call
// the way the rest of this fake's surface is.
type fileFixture struct {
	mime string
	data []byte
}

// SeedFile registers fixture bytes for an inbound Slack file attachment and
// returns the slackapi.File descriptor to pass to InjectDMWithFiles (the
// AppMentionEvent path takes []slackapi.File directly). Lazily starts the
// fake's download server on first call — most e2e scenarios never attach a
// file, so paying for an httptest.Server only when one actually seeds a file
// keeps every other scenario's Start cost unchanged.
func (c *Client) SeedFile(fileID, filename, mime string, data []byte) slackapi.File {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = map[string]*fileFixture{}
	}
	c.files[fileID] = &fileFixture{mime: mime, data: data}
	if c.downloadSrv == nil {
		c.downloadSrv = httptest.NewServer(http.HandlerFunc(c.serveDownload))
	}
	return slackapi.File{ID: fileID, Name: filename, Mimetype: mime, Size: len(data)}
}

// serveDownload answers the private-download leg of FetchAttachment: a plain
// GET whose path is the file ID, returning exactly the bytes SeedFile
// registered. Unlike this package's other simulated behavior, this route is
// real HTTP — FetchAttachment builds an *http.Request by hand against
// file.URLPrivateDownload and cannot be redirected through a Go-level fake.
func (c *Client) serveDownload(w http.ResponseWriter, r *http.Request) {
	fileID := strings.TrimPrefix(r.URL.Path, "/")
	c.mu.Lock()
	f, ok := c.files[fileID]
	c.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.mime)
	_, _ = w.Write(f.data)
}

// GetFileInfoContext implements attachmentInfoClient
// (pkg/channels/channelkinds/slack/attachments.go): the files.info resolve step
// FetchAttachment's first leg makes. Returns a File descriptor whose
// URLPrivateDownload points at this fake's own download server — real
// slack.com URLs are never reachable from a test process.
func (c *Client) GetFileInfoContext(_ context.Context, fileID string, _, _ int) (*slackapi.File, []slackapi.Comment, *slackapi.Paging, error) {
	c.mu.Lock()
	f, ok := c.files[fileID]
	srv := c.downloadSrv
	c.mu.Unlock()
	if !ok || srv == nil {
		return nil, nil, nil, fmt.Errorf("fakeslack: no file %q seeded (call Client.SeedFile before injecting a message that attaches it)", fileID)
	}
	return &slackapi.File{
		ID:                 fileID,
		Mimetype:           f.mime,
		Size:               len(f.data),
		URLPrivateDownload: srv.URL + "/" + fileID,
	}, nil, nil, nil
}

// Close shuts down the fake's lazily-started file-download server. Safe to
// call even when no file was ever seeded (SeedFile was never called, so no
// server exists). Wired via t.Cleanup by the e2e harness's Start.
func (c *Client) Close() {
	c.mu.Lock()
	srv := c.downloadSrv
	c.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}
