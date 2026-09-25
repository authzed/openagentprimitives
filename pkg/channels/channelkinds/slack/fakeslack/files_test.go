package fakeslack

import (
	"context"
	"io"
	"net/http"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSeedFile_GetFileInfoAndDownloadRoundtrip proves the two-leg fetch
// FetchAttachment (pkg/channels/channelkinds/slack/attachments.go) performs against a
// real Slack file: files.info (GetFileInfoContext) resolves a private
// download URL, and a plain GET against that URL returns exactly the seeded
// bytes.
func TestSeedFile_GetFileInfoAndDownloadRoundtrip(t *testing.T) {
	c := New()
	t.Cleanup(c.Close)

	want := []byte("fake docx bytes")
	desc := c.SeedFile("F1", "report.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", want)
	assert.Equal(t, "F1", desc.ID)
	assert.Equal(t, "report.docx", desc.Name)
	assert.Equal(t, len(want), desc.Size)

	file, _, _, err := c.GetFileInfoContext(context.Background(), "F1", 0, 0)
	require.NoError(t, err)
	require.NotNil(t, file)
	require.NotEmpty(t, file.URLPrivateDownload, "FetchAttachment refuses a file with no private download URL")

	resp, err := http.Get(file.URLPrivateDownload) //nolint:noctx // test-only fixture fetch
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestGetFileInfoContext_UnseededFileErrors proves a file ID that was never
// seeded fails closed with a clear error rather than a nil-pointer panic or a
// silently-empty download URL — a test that forgets SeedFile should fail
// loudly, not flake on a 404 from a stray real network call.
func TestGetFileInfoContext_UnseededFileErrors(t *testing.T) {
	c := New()
	t.Cleanup(c.Close)

	file, _, _, err := c.GetFileInfoContext(context.Background(), "F-never-seeded", 0, 0)
	require.Error(t, err)
	assert.Nil(t, file)
}

// TestClose_NoopWhenNoFileWasEverSeeded proves Close tolerates the common
// case (a scenario that never calls SeedFile, so no download server was ever
// started) — the e2e harness calls this unconditionally in Start's cleanup.
func TestClose_NoopWhenNoFileWasEverSeeded(t *testing.T) {
	c := New()
	assert.NotPanics(t, c.Close)
}

// TestInjectDMWithFiles_BuildsFileShareEventListenerAdmits proves the event
// shape the real listener requires to record an attachment on a DM: SubType
// == file_share (the one admitted exception to the subtype gate — see
// listener.go's comment on the *slackevents.MessageEvent case) and the files
// riding under Message.Files (NOT the top-level Files field
// AppMentionEvent uses — see slackMessageEventFiles's doc in listener.go).
func TestInjectDMWithFiles_BuildsFileShareEventListenerAdmits(t *testing.T) {
	c := New()
	t.Cleanup(c.Close)

	file := c.SeedFile("F1", "report.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", []byte("bytes"))
	ts, ev := c.InjectDMWithFiles("U1", "D01", "check this out", []slackapi.File{file})
	require.NotEmpty(t, ts)

	top := c.TopLevel("D01")
	require.Len(t, top, 1)
	assert.Equal(t, slackapi.MsgSubTypeFileShare, top[0].SubType)

	require.Equal(t, socketmode.EventTypeEventsAPI, ev.Type)
	api, ok := ev.Data.(slackevents.EventsAPIEvent)
	require.True(t, ok)
	msg, ok := api.InnerEvent.Data.(*slackevents.MessageEvent)
	require.True(t, ok)
	assert.Equal(t, slackapi.MsgSubTypeFileShare, msg.SubType)
	assert.Equal(t, "im", msg.ChannelType)
	require.NotNil(t, msg.Message, "Files must ride under the nested Message.Files, not the top-level field")
	require.Len(t, msg.Message.Files, 1)
	assert.Equal(t, "F1", msg.Message.Files[0].ID)
}
