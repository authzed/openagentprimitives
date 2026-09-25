package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// --- sanitizeAttachmentFilename -------------------------------------------

func TestSanitizeAttachmentFilename(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain name passes through unchanged", input: "report.pdf", want: "report.pdf"},
		{name: "control characters are stripped", input: "evil\x00\x1fname\x7f.txt", want: "evilname.txt"},
		{name: "embedded newline/tab are stripped (log-line safety)", input: "line1\nline2\ttabbed", want: "line1line2tabbed"},
		{name: "empty input stays empty", input: "", want: ""},
		{name: "all-control input becomes empty, not skipped", input: "\x00\x01\x02", want: ""},
		{name: "multi-byte runes survive intact (not byte-truncated mid-codepoint)", input: "日本語ファイル名.txt", want: "日本語ファイル名.txt"},
		{
			name:  "truncates to 128 surviving runes, counting only non-control runes",
			input: strings.Repeat("a", 130),
			want:  strings.Repeat("a", 128),
		},
		{
			name:  "truncation counts survivors only — stripped control chars don't consume the budget",
			input: strings.Repeat("\x00", 50) + strings.Repeat("b", 128),
			want:  strings.Repeat("b", 128),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeAttachmentFilename(tc.input)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len([]rune(got)), maxAttachmentFilenameRunes)
		})
	}
}

// --- slackInboundAttachments -----------------------------------------------

func TestSlackInboundAttachments(t *testing.T) {
	t.Run("nil/empty input yields nil, not an empty allocated slice", func(t *testing.T) {
		assert.Nil(t, slackInboundAttachments(nil))
		assert.Nil(t, slackInboundAttachments([]slackapi.File{}))
	})

	t.Run("maps ID/Name/Mimetype/Size and sanitizes the name", func(t *testing.T) {
		got := slackInboundAttachments([]slackapi.File{
			{ID: "F1", Name: "evil\x00name.pdf", Mimetype: "application/pdf", Size: 4096},
			{ID: "F2", Name: "photo.png", Mimetype: "image/png", Size: 1024},
		})
		require.Len(t, got, 2)
		assert.Equal(t, channelkinds.InboundAttachment{
			ExternalID: "F1", Filename: "evilname.pdf", MIME: "application/pdf", SizeBytes: 4096,
		}, got[0])
		assert.Equal(t, channelkinds.InboundAttachment{
			ExternalID: "F2", Filename: "photo.png", MIME: "image/png", SizeBytes: 1024,
		}, got[1])
	})
}

// --- listener: ungated population ------------------------------------------

// newAttachTestListener builds a minimal slackListener whose pipeline is a
// recordingInbound (defined in fakeslack_listener_dispatch_test.go), with the
// identity cache pre-primed so resolveIdentity needs no live API call. No
// AttachmentFetcher, Channel spec, or capability of any kind is wired here —
// that absence is the point of the tests below: recording that a file
// exists must not depend on any of it.
func newAttachTestListener(t *testing.T, got *channelkinds.InboundEvent) *slackListener {
	t.Helper()
	return &slackListener{
		deps: channelkinds.Deps{
			Inbound: recordingInbound{
				dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted},
				got: got,
			},
		},
		seenEvts:         newEventIDCache(64),
		threads:          newThreadIndex(),
		assistantThreads: map[string]string{},
		idents:           NewIdentityCache(8),
		botUserID:        "UBOT",
	}
}

// dispatchInner wraps an Events API inner event in the socketmode.Event
// envelope l.handle expects and drives it through the REAL dispatch chain
// (l.handle -> l.handleEventsAPI -> handleDM/handleChannelMessage), the same
// path production socket-mode delivery uses.
func dispatchInner(l *slackListener, innerType string, data interface{}) {
	l.handle(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{
			Type: slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Type: innerType,
				Data: data,
			},
		},
	})
}

// TestListener_AppMention_PopulatesAttachmentsUngated pins the design point:
// the listener records that a file was attached with NO capability check
// anywhere in this test's setup. Whether the bytes are ever fetched is decided
// downstream; noticing the file exists is unconditional.
func TestListener_AppMention_PopulatesAttachmentsUngated(t *testing.T) {
	var got channelkinds.InboundEvent
	l := newAttachTestListener(t, &got)

	dispatchInner(l, "app_mention", &slackevents.AppMentionEvent{
		User:      "U1",
		Channel:   "C1",
		TimeStamp: "100.001",
		Text:      "<@UBOT> look at this",
		Files: []slackapi.File{
			{ID: "F1", Name: "report.pdf", Mimetype: "application/pdf", Size: 4096},
		},
	})

	require.Len(t, got.Attachments, 1, "no AttachmentFetcher/capability was configured anywhere in this test — the listener must still record the file")
	assert.Equal(t, channelkinds.InboundAttachment{
		ExternalID: "F1", Filename: "report.pdf", MIME: "application/pdf", SizeBytes: 4096,
	}, got.Attachments[0])
}

// TestListener_DMFileShare_PopulatesAttachmentsUngated exercises the DM path,
// where Slack tags the inbound message with subtype "file_share" — the
// listener's SubType gate must admit that subtype (see the comment on the
// *slackevents.MessageEvent case in listener.go) or the file never reaches
// InboundEvent at all, regardless of anything this test wires up.
func TestListener_DMFileShare_PopulatesAttachmentsUngated(t *testing.T) {
	var got channelkinds.InboundEvent
	l := newAttachTestListener(t, &got)

	dispatchInner(l, "message", &slackevents.MessageEvent{
		Type:        "message",
		SubType:     slackapi.MsgSubTypeFileShare,
		User:        "U1",
		Channel:     "D1",
		ChannelType: "im",
		TimeStamp:   "200.001",
		Text:        "",
		Message: &slackapi.Msg{
			Files: []slackapi.File{
				{ID: "F2", Name: "notes.txt", Mimetype: "text/plain", Size: 128},
			},
		},
	})

	require.Len(t, got.Attachments, 1, "a bare file_share DM (no text) must still be recorded, not dropped")
	assert.Equal(t, channelkinds.InboundAttachment{
		ExternalID: "F2", Filename: "notes.txt", MIME: "text/plain", SizeBytes: 128,
	}, got.Attachments[0])
	assert.Equal(t, "dm:U1", got.ChannelKey, "the file_share subtype exception must not change DM routing")
}

// TestListener_MessageEvent_NonFileShareSubtypesStillRejected guards the
// SubType-gate change: file_share is the ONE admitted exception. Every other
// subtype (edits, membership churn, bot posts, ...) must still be dropped
// before Deliver is ever called.
func TestListener_MessageEvent_NonFileShareSubtypesStillRejected(t *testing.T) {
	subtypes := []string{
		slackapi.MsgSubTypeChannelJoin,
		slackapi.MsgSubTypeMessageChanged,
		slackapi.MsgSubTypeBotMessage,
	}
	for _, st := range subtypes {
		t.Run(st, func(t *testing.T) {
			var got channelkinds.InboundEvent
			l := newAttachTestListener(t, &got)

			dispatchInner(l, "message", &slackevents.MessageEvent{
				Type:        "message",
				SubType:     st,
				User:        "U1",
				Channel:     "D1",
				ChannelType: "im",
				TimeStamp:   "300.001",
				Message:     &slackapi.Msg{},
			})

			assert.Equal(t, channelkinds.InboundEvent{}, got, "subtype %q must never reach Deliver", st)
		})
	}
}

// --- AttachmentBounds --------------------------------------------------

func TestAttachmentBounds(t *testing.T) {
	k := &Kind{}
	got := k.AttachmentBounds()
	assert.Equal(t, int64(1<<30), got.MaxSizeBytes, "Slack's practical per-file ceiling is 1 GiB")
	assert.Equal(t, 10, got.MaxPerMessage)
}

// --- FetchAttachment ---------------------------------------------------

const attachTestBotToken = "xoxb-attach-test-token"

func attachTestDeps() channelkinds.Deps {
	return channelkinds.Deps{
		Secret: &corev1.Secret{
			Data: map[string][]byte{
				SecretKeyBotToken: []byte(attachTestBotToken),
			},
		},
	}
}

// installAttachmentInfoClient overrides attachmentInfoClientFactory so
// files.info resolves against apiURL instead of slack.com, restoring the
// original factory on test cleanup.
func installAttachmentInfoClient(t *testing.T, apiURL string) {
	t.Helper()
	orig := attachmentInfoClientFactory
	attachmentInfoClientFactory = func(channelkinds.Deps) attachmentInfoClient {
		return slackapi.New(attachTestBotToken, slackapi.OptionAPIURL(apiURL+"/"))
	}
	t.Cleanup(func() { attachmentInfoClientFactory = orig })
}

// attachmentTestServer serves TWO distinct routes: files.info, which always
// succeeds and stays identical across every test case, and the download
// leg, whose behavior downloadHandler controls per case. Per the repo's
// MCP-fake lesson, a fake that faults every request uniformly can't prove a
// bug lives specifically on the download leg — keeping files.info constant
// while only the download leg varies is what makes that provable.
func attachmentTestServer(t *testing.T, fileID string, downloadHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/files.info", func(w http.ResponseWriter, r *http.Request) {
		// slack-go's files.info call authenticates via a "token" form field
		// (postForm), not an Authorization header — unlike the download leg
		// below, which this package builds by hand specifically to set
		// Authorization: Bearer. Checking the form field here proves the same
		// bot-token client is used for the resolve step.
		require.NoError(t, r.ParseForm())
		assert.Equal(t, attachTestBotToken, r.FormValue("token"),
			"files.info must authenticate with the same bot token")
		fmt.Fprintf(w, `{"ok":true,"file":{"id":%q,"url_private_download":%q}}`,
			fileID, srv.URL+"/download/"+fileID)
	})
	mux.HandleFunc("/download/"+fileID, downloadHandler)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchAttachment_200StreamsWithoutBuffering(t *testing.T) {
	const fileID = "F200"
	release := make(chan struct{})

	downloadHandler := func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+attachTestBotToken, r.Header.Get("Authorization"),
			"the bot token must be present on the download request specifically")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "httptest ResponseWriter must support Flush for this test")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first-chunk-"))
		flusher.Flush()
		<-release // held open until the test explicitly lets it finish
		_, _ = w.Write([]byte("second-chunk"))
	}
	srv := attachmentTestServer(t, fileID, downloadHandler)
	installAttachmentInfoClient(t, srv.URL)

	k := &Kind{}
	rc, err := k.FetchAttachment(context.Background(), attachTestDeps(), fileID)
	require.NoError(t, err)
	// FetchAttachment has already returned, even though the handler is still
	// parked on <-release (nothing has closed it yet). If FetchAttachment
	// had buffered the whole body — e.g. io.ReadAll — before returning, this
	// line would never be reached: the call above would block forever
	// waiting for a second chunk the handler can't send until release closes.
	close(release)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "first-chunk-second-chunk", string(data))
	assert.NoError(t, rc.Close(), "the returned reader must be closeable by the caller without leaking")
}

func TestFetchAttachment_DownloadErrors(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantInErr  string
	}{
		{name: "404 names the status", statusCode: http.StatusNotFound, wantInErr: "404"},
		{name: "500 names the status", statusCode: http.StatusInternalServerError, wantInErr: "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fileID := fmt.Sprintf("FERR%d", tc.statusCode)
			downloadHandler := func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer "+attachTestBotToken, r.Header.Get("Authorization"))
				w.WriteHeader(tc.statusCode)
			}
			srv := attachmentTestServer(t, fileID, downloadHandler)
			installAttachmentInfoClient(t, srv.URL)

			k := &Kind{}
			rc, err := k.FetchAttachment(context.Background(), attachTestDeps(), fileID)
			require.Error(t, err)
			assert.Nil(t, rc)
			assert.Contains(t, err.Error(), tc.wantInErr)
		})
	}
}

func TestFetchAttachment_NoBotToken(t *testing.T) {
	k := &Kind{}
	rc, err := k.FetchAttachment(context.Background(), channelkinds.Deps{}, "F1")
	require.Error(t, err)
	assert.Nil(t, rc)
}

// --- attachmentInfoClientFactory's testClientOverride branch ---------------

// TestAttachmentInfoClientFactory_ReturnsInstalledFake proves the
// testClientOverride branch added to attachmentInfoClientFactory (this
// file's package doc) actually does what the e2e harness depends on: once
// InstallTestTransport has installed a shared fakeslack.Client, the DEFAULT
// factory — the one production and every e2e scenario actually calls, not
// the fully-replaced installAttachmentInfoClient override used elsewhere in
// this file — must return that exact instance, never fall through to
// `slackapi.New(tok)` (which would dial real slack.com). Before this branch
// existed, no test exercised this path at all: attachmentInfoClientFactory
// was invisible to InstallTestTransport entirely.
func TestAttachmentInfoClientFactory_ReturnsInstalledFake(t *testing.T) {
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	reset := InstallTestTransport(fake, fakeslack.NewSocketSource())
	t.Cleanup(reset)

	got := attachmentInfoClientFactory(attachTestDeps())
	assert.Same(t, fake, got, "attachmentInfoClientFactory must return the installed fake instance, not a real slack.com client")
}

// TestAttachmentInfoClientFactory_FallsThroughWhenOverrideLacksTheInterface
// pins the OTHER half of that branch's contract, symmetric with
// sender_test.go's/testhooks_test.go's coverage of newSlackAPIClient's
// identical comma-ok pattern: an installed override that does NOT implement
// attachmentInfoClient (stubClient, from testhooks_test.go — it has no
// GetFileInfoContext method) must fall through to the production
// token-based path rather than returning a nil or zero-value client. This is
// "the exact bug class that made the [testClientOverride] change necessary"
// — pinning it here means a future override type that silently stops
// satisfying attachmentInfoClient fails a targeted test instead of only
// surfacing as a live network call to slack.com in some later e2e run.
func TestAttachmentInfoClientFactory_FallsThroughWhenOverrideLacksTheInterface(t *testing.T) {
	reset := InstallTestTransport(&stubClient{}, stubSource{})
	t.Cleanup(reset)

	got := attachmentInfoClientFactory(attachTestDeps())
	require.NotNil(t, got, "a valid bot-token Secret must still produce a client on fallthrough")
	_, isRealClient := got.(*slackapi.Client)
	assert.True(t, isRealClient, "an override that doesn't implement attachmentInfoClient must fall through to a real slackapi.Client")
}
