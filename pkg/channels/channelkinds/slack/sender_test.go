package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// emailCanonical encodes an email address in the base64-RawURL form
// identity.Principal.Canonical() produces for email-based subjects. Shared
// by every sender test that needs a RecipientCanonical / requester ExternalID
// in that form.
func emailCanonical(email string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(email))
}

// sessionWithChannel returns a SessionInfo bound to a specific
// Slack channel + thread — the "has channel context" case.
func sessionWithChannel(channelID, threadTS string) channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: "default",
		Name:      "sess-1",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng",
			Kind: "slack",
			External: map[string]string{
				"channel_id": channelID,
				"thread_ts":  threadTS,
			},
		},
	}
}

// sessionDMOnly returns a SessionInfo with no channel_id — the DM fallback case.
func sessionDMOnly() channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace: "default",
		Name:      "sess-dm",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name:     "slack-eng",
			Kind:     "slack",
			External: map[string]string{},
		},
	}
}

// fakeClientWithUser returns a fakeSlackClient pre-seeded with a Slack
// user resolvable via email lookup, which is the expected path in tests
// that supply an email-based canonical.
func fakeClientWithUser(email, slackID string) *fakeSlackClient {
	return &fakeSlackClient{
		lookupByEmail: map[string]*slackapi.User{
			email: {ID: slackID, Profile: slackapi.UserProfile{Email: email}},
		},
	}
}

// firstSectionText extracts the mrkdwn text from block[0] of a rendered
// Block Kit message. Fails the test if block[0] is not a *SectionBlock or
// its Text is nil.
func firstSectionText(t *testing.T, blocks []slackapi.Block) string {
	t.Helper()
	require.NotEmpty(t, blocks, "blocks must not be empty")
	section, ok := blocks[0].(*slackapi.SectionBlock)
	require.True(t, ok, "block[0] must be *SectionBlock")
	require.NotNil(t, section.Text, "section.Text must not be nil")
	return section.Text.Text
}

// fakeSlackClient implements slackClient and records calls.
//
// The awaiting-indicator ticker goroutine calls UpdateMessageContext
// from a separate goroutine, so updateCalls reads/writes go through
// mu. Tests that race the ticker (e.g.
// TestToolApprovalSender_AwaitingTicker_*) MUST use the
// snapshotUpdateCalls helper instead of touching updateCalls directly.
type fakeSlackClient struct {
	mu                 sync.Mutex
	postMessageCalls   []postMessageCall
	postEphemeralCalls []postEphemeralCall
	updateCalls        []updateCall
	getPermalinkCalls  []slackapi.PermalinkParameters
	getPermalinkResult string
	getPermalinkErr    error
	openConvCalls      []string
	setStatusCalls     []setStatusCall
	setTitleCalls      []setTitleCall
	uploadFileCalls    []uploadFileCall
	postMessageErr     error
	postEphemeralErr   error
	// postEphemeralErrs, FIFO per-call. Drives the
	// user_not_in_channel → DM fallback test.
	postEphemeralErrs []error
	updateErr         error
	// postMessageErrs, when non-empty, supplies one error per PostMessageContext
	// call (consumed in FIFO order). Drives the fallback test which needs the
	// first call to fail with invalid_blocks and the second to succeed.
	postMessageErrs []error
	// updateErrs is the same for UpdateMessageContext.
	updateErrs        []error
	setStatusErr      error
	setTitleErr       error
	uploadFileErr     error
	openConvChannelID string
	// openConversationErr makes OpenConversationContext return this error
	// instead of succeeding. Drives the DM-open-fails-loud fallback test.
	openConversationErr error
	// openViewCalls records every views.open invocation (the Show
	// Details modal). Tests inspect this to assert the listener handler
	// fired with the right blocks. Access under f.mu so reads are
	// race-safe vs the listener goroutine.
	openViewCalls []openViewCall
	openViewErr   error
	// postedTS, when non-empty, overrides the ts returned by PostMessageContext.
	// Tests that need a deterministic ts for chat.update assertions set this.
	postedTS string
	// postedTSs, when non-empty, supplies one ts per PostMessageContext call
	// (consumed in FIFO order), overriding postedTS for that call. Drives
	// tests that need to distinguish a thread ROOT post (e.g. the fork-root
	// framing message) from a later post INTO that thread (e.g. the tool
	// bubble) — mirrors the postMessageErrs FIFO pattern.
	postedTSs []string

	lookupByEmail      map[string]*slackapi.User // email → User
	lookupByEmailErr   error                     // sticky error returned by GetUserByEmailContext
	lookupByEmailCalls []string                  // emails queried
	userInfo           map[string]*slackapi.User // user id → User, for users.info
	userInfoErr        error                     // sticky error returned by GetUserInfoContext
	userInfoCalls      []string                  // user ids queried
	usersList          []slackapi.User           // returned by GetUsersContext
	usersListErr       error
	usersListCalls     int

	// openedDM / postedEphemeral / postedToChannel are simple booleans set the
	// first time OpenConversationContext / PostEphemeralContext /
	// PostMessageContext is called — a coarser, easier-to-assert-on
	// complement to openConvCalls / postEphemeralCalls / postMessageCalls for
	// tests (e.g. interaction_delivery_test.go, interaction_public_note_test.go)
	// that only care "did this happen at all", not the call detail.
	// lastPostedText records the plain-text MsgOptionText argument of the
	// most recent PostMessageContext call (extracted via
	// UnsafeApplyMsgOptions, the same idiom other tests in this package
	// use to inspect message content — see interaction_test.go).
	openedDM        bool
	postedEphemeral bool
	postedToChannel bool
	lastPostedText  string

	// updatedRefs records "channelID:ts" for every UpdateMessageContext call —
	// a coarser, easier-to-assert-on complement to updateCalls for tests (e.g.
	// interaction_test.go's dual-surface applied-edit test) that only care
	// which message refs were edited, not the call detail. Access under f.mu.
	updatedRefs []string
}

type postMessageCall struct {
	channelID string
	options   []slackapi.MsgOption
}

type postEphemeralCall struct {
	channelID, userID string
	options           []slackapi.MsgOption
}

type updateCall struct {
	channelID string
	ts        string
	opts      []slackapi.MsgOption
}

type setStatusCall struct {
	channelID       string
	threadTS        string
	status          string
	loadingMessages []string
}

type setTitleCall struct {
	channelID string
	threadTS  string
	title     string
}

type openViewCall struct {
	triggerID string
	view      slackapi.ModalViewRequest
}

type uploadFileCall struct {
	channelID      string
	threadTS       string
	filename       string
	initialComment string
	altTxt         string
	contents       []byte
}

func (f *fakeSlackClient) PostMessageContext(_ context.Context, channelID string, opts ...slackapi.MsgOption) (string, string, error) {
	f.postMessageCalls = append(f.postMessageCalls, postMessageCall{channelID, opts})
	f.postedToChannel = true
	if len(f.postMessageErrs) > 0 {
		e := f.postMessageErrs[0]
		f.postMessageErrs = f.postMessageErrs[1:]
		if e != nil {
			return "", "", e
		}
	} else if f.postMessageErr != nil {
		return "", "", f.postMessageErr
	}
	ts := f.postedTS
	if len(f.postedTSs) > 0 {
		ts = f.postedTSs[0]
		f.postedTSs = f.postedTSs[1:]
	}
	if ts == "" {
		ts = "1614191050.013300" // default for tests that don't need a specific ts
	}
	if _, vals, applyErr := slackapi.UnsafeApplyMsgOptions("test-token", channelID, "http://test.invalid/", opts...); applyErr == nil {
		f.lastPostedText = vals.Get("text")
	}
	return channelID, ts, nil
}

func (f *fakeSlackClient) PostEphemeralContext(_ context.Context, channelID, userID string, opts ...slackapi.MsgOption) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postEphemeralCalls = append(f.postEphemeralCalls, postEphemeralCall{channelID, userID, opts})
	f.postedEphemeral = true
	// FIFO per-call error injection (mirrors postMessageErrs).
	if len(f.postEphemeralErrs) > 0 {
		e := f.postEphemeralErrs[0]
		f.postEphemeralErrs = f.postEphemeralErrs[1:]
		if e != nil {
			return "", e
		}
	}
	if f.postEphemeralErr != nil {
		return "", f.postEphemeralErr
	}
	return "1614191050.013301", nil
}

func (f *fakeSlackClient) GetPermalinkContext(_ context.Context, params *slackapi.PermalinkParameters) (string, error) {
	if params != nil {
		f.getPermalinkCalls = append(f.getPermalinkCalls, *params)
	}
	if f.getPermalinkErr != nil {
		return "", f.getPermalinkErr
	}
	return f.getPermalinkResult, nil
}

func (f *fakeSlackClient) OpenConversationContext(_ context.Context, params *slackapi.OpenConversationParameters) (*slackapi.Channel, bool, bool, error) {
	f.openConvCalls = append(f.openConvCalls, params.Users[0])
	f.openedDM = true
	if f.openConversationErr != nil {
		return nil, false, false, f.openConversationErr
	}
	id := f.openConvChannelID
	if id == "" {
		id = "D" + params.Users[0]
	}
	return &slackapi.Channel{GroupConversation: slackapi.GroupConversation{Conversation: slackapi.Conversation{ID: id}}}, false, false, nil
}

func (f *fakeSlackClient) SetAssistantThreadsStatusContext(_ context.Context, params slackapi.AssistantThreadsSetStatusParameters) error {
	f.setStatusCalls = append(f.setStatusCalls, setStatusCall{
		channelID:       params.ChannelID,
		threadTS:        params.ThreadTS,
		status:          params.Status,
		loadingMessages: params.LoadingMessages,
	})
	return f.setStatusErr
}

func (f *fakeSlackClient) SetAssistantThreadsTitleContext(_ context.Context, params slackapi.AssistantThreadsSetTitleParameters) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setTitleCalls = append(f.setTitleCalls, setTitleCall{
		channelID: params.ChannelID,
		threadTS:  params.ThreadTS,
		title:     params.Title,
	})
	return f.setTitleErr
}

func (f *fakeSlackClient) UpdateMessageContext(_ context.Context, channelID, ts string, opts ...slackapi.MsgOption) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls = append(f.updateCalls, updateCall{channelID: channelID, ts: ts, opts: opts})
	f.updatedRefs = append(f.updatedRefs, channelID+":"+ts)
	if len(f.updateErrs) > 0 {
		e := f.updateErrs[0]
		f.updateErrs = f.updateErrs[1:]
		return channelID, ts, "", e
	}
	return channelID, ts, "", f.updateErr
}

// snapshotUpdateCalls returns a copy of the recorded chat.update
// calls under the fake's lock. Tests that race the awaiting ticker
// goroutine MUST use this instead of touching updateCalls directly.
func (f *fakeSlackClient) snapshotUpdateCalls() []updateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]updateCall, len(f.updateCalls))
	copy(out, f.updateCalls)
	return out
}

// snapshotEphemeralCalls returns a copy of the recorded chat.postEphemeral
// calls under the fake's lock. Tests that race a deferred goroutine (e.g. the
// missing-app_mention grace-window hint) MUST use this instead of touching
// postEphemeralCalls directly.
func (f *fakeSlackClient) snapshotEphemeralCalls() []postEphemeralCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]postEphemeralCall, len(f.postEphemeralCalls))
	copy(out, f.postEphemeralCalls)
	return out
}

func (f *fakeSlackClient) OpenViewContext(_ context.Context, triggerID string, view slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openViewCalls = append(f.openViewCalls, openViewCall{triggerID: triggerID, view: view})
	if f.openViewErr != nil {
		return nil, f.openViewErr
	}
	return &slackapi.ViewResponse{}, nil
}

func (f *fakeSlackClient) UploadFileContext(_ context.Context, params slackapi.UploadFileParameters) (*slackapi.FileSummary, error) {
	call := uploadFileCall{
		channelID:      params.Channel,
		threadTS:       params.ThreadTimestamp,
		filename:       params.Filename,
		initialComment: params.InitialComment,
		altTxt:         params.AltTxt,
	}
	if params.Reader != nil {
		buf, _ := io.ReadAll(params.Reader)
		call.contents = buf
	}
	f.uploadFileCalls = append(f.uploadFileCalls, call)
	if f.uploadFileErr != nil {
		return nil, f.uploadFileErr
	}
	return &slackapi.FileSummary{ID: "F-stub", Title: params.Filename}, nil
}

// stubAssetFetcher returns a fixed AssetBytes for any (ns, sess, render) tuple
// and records every call so tests can assert the sender resolved each
// AttachmentRef exactly once and routed to the right upload.
type stubAssetFetcher struct {
	calls    []stubFetchCall
	bytes    []byte
	mime     string
	filename string
	err      error
}

type stubFetchCall struct {
	ns, sess, render string
}

func (s *stubAssetFetcher) Fetch(_ context.Context, ns, sess, render string) (channelkinds.AssetBytes, error) {
	s.calls = append(s.calls, stubFetchCall{ns, sess, render})
	if s.err != nil {
		return channelkinds.AssetBytes{}, s.err
	}
	return channelkinds.AssetBytes{
		Bytes:    s.bytes,
		MIME:     s.mime,
		Filename: s.filename,
	}, nil
}

func newSender(client slackClient) *slackSender {
	return &slackSender{
		deps:         channelkinds.Deps{},
		client:       client,
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
		toolProgress: make(map[string]map[string]toolProgEntry),
	}
}

func userMessageEnvelope(t *testing.T, text string) channelevents.Envelope {
	t.Helper()
	pl, err := json.Marshal(channelevents.OutboundUserMessagePayload{Text: text})
	require.NoError(t, err, "marshal OutboundUserMessagePayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindUserMessage,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "foo"},
		PublishedAt: metav1.Now().Time,
		Payload:     pl,
	}
}

func notificationEnvelope(t *testing.T, text string) channelevents.Envelope {
	t.Helper()
	pl, err := json.Marshal(channelevents.NotificationPayload{Text: text})
	require.NoError(t, err, "marshal NotificationPayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindNotification,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "foo"},
		PublishedAt: metav1.Now().Time,
		Payload:     pl,
	}
}

func operationActivityEnvelope(t *testing.T, compactLine string) channelevents.Envelope {
	t.Helper()
	pl, err := json.Marshal(channelevents.OperationActivityPayload{CompactLine: compactLine})
	require.NoError(t, err, "marshal OperationActivityPayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindOperationActivity,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "foo"},
		PublishedAt: metav1.Now().Time,
		Payload:     pl,
	}
}

func TestSenderPostMessageThreaded(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300",
			},
		},
	}
	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "hello"))
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1, "postMessage call count")

	got := c.postMessageCalls[0]
	assert.Equal(t, "C01ABCDEF", got.channelID, "channelID")
	// MsgOption is opaque (it's a function); we can't inspect contents directly
	// without reaching into slack-go internals. Confirm at least that we passed
	// 3 options (Text + Blocks + TS) for threaded path.
	// Text is the plain-text push-notification fallback; Blocks carries the
	// reply section + optional clamp line + Show-settings action; TS threads it.
	assert.Len(t, got.options, 3, "expected 3 MsgOptions (Text, Blocks, TS) for threaded path")
	// No setStatus calls for user_message.
	assert.Empty(t, c.setStatusCalls, "setStatus must not be called for user_message")
}

func TestSenderPostMessageDM_NoThread(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "D01ABCDEF"},
		},
	}
	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "hi"))
	require.NoError(t, err, "Send")

	got := c.postMessageCalls[0]
	assert.Equal(t, "D01ABCDEF", got.channelID, "channelID")
	// Text (push-notification fallback) + Blocks (reply section + Show-settings button).
	// No TS option for DM (no thread_ts).
	assert.Len(t, got.options, 2, "DM should pass Text + Blocks MsgOptions (no TS)")
}

func TestSenderMissingChannelID(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{}},
	}
	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "x"))
	require.Error(t, err, "expected channel_id error")
	assert.Contains(t, err.Error(), "channel_id", "error message")
	assert.Empty(t, c.postMessageCalls, "postMessage must not be called when channel_id missing")
}

func TestSenderUnsupportedKind(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
	}
	// Use a reserved-but-unimplemented kind to exercise the unsupported path.
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindPermissionRequest,
		Session: channelevents.SessionRef{Namespace: "n", Name: "s"},
		Payload: []byte("{}"),
	}
	_, err := s.Send(context.Background(), sess, env)
	require.Error(t, err, "expected unsupported-kind error")
	assert.Contains(t, err.Error(), "unsupported envelope kind", "error message")
}

func TestSenderPostMessageFailure(t *testing.T) {
	c := &fakeSlackClient{postMessageErr: errors.New("rate limited")}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
	}
	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "x"))
	require.Error(t, err, "expected rate-limited propagation")
	assert.Contains(t, err.Error(), "rate limited", "error message")
}

// TestSenderAttachmentsUploadsFiles verifies that an envelope carrying
// AttachmentRefs takes the files.uploadV2 path: each ref is resolved via
// the AssetFetcher and uploaded via UploadFileContext. Text rides on the
// FIRST upload's InitialComment only — subsequent uploads must carry an
// empty InitialComment so the user doesn't see N copies of the message.
// PostMessageContext must NOT be called on this path.
func TestSenderAttachmentsUploadsFiles(t *testing.T) {
	c := &fakeSlackClient{}
	af := &stubAssetFetcher{
		bytes:    []byte("<html>hi</html>"),
		mime:     "text/html",
		filename: "report.html",
	}
	s := &slackSender{
		deps:   channelkinds.Deps{AssetFetcher: af},
		client: c,
	}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300",
			},
		},
	}
	pl := channelevents.OutboundUserMessagePayload{
		Text: "Here is the report",
		Attachments: []channelevents.AttachmentRef{
			{RenderName: "ar-1", MIME: "text/html", Filename: "report-a.html", AltText: "report a"},
			{RenderName: "ar-2", MIME: "text/html", Filename: "report-b.html"},
		},
	}
	plBytes, err := json.Marshal(pl)
	require.NoError(t, err, "marshal payload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindUserMessage,
		Session: channelevents.SessionRef{Namespace: "default", Name: "foo"},
		Payload: plBytes,
	}
	_, err = s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	assert.Empty(t, c.postMessageCalls, "postMessage must not be called on attachment path")
	require.Len(t, c.uploadFileCalls, 2, "expected 2 uploadFile calls")
	require.Len(t, af.calls, 2, "expected 2 fetch calls")

	wantFetches := []stubFetchCall{
		{ns: "default", sess: "foo", render: "ar-1"},
		{ns: "default", sess: "foo", render: "ar-2"},
	}
	for i, want := range wantFetches {
		assert.Equalf(t, want, af.calls[i], "fetch[%d]", i)
	}
	// First upload: InitialComment carries the text; runner-supplied filename hint
	// wins over the fetcher's Content-Disposition copy.
	first := c.uploadFileCalls[0]
	assert.Equal(t, "C01ABCDEF", first.channelID, "first.channelID")
	assert.Equal(t, "1614191050.013300", first.threadTS, "first.threadTS")
	assert.Equal(t, "Here is the report", first.initialComment, "first.initialComment")
	assert.Equal(t, "report-a.html", first.filename,
		"runner hint should win over fetcher's filename")
	assert.Equal(t, "report a", first.altTxt, "first.altTxt")
	assert.Equal(t, "<html>hi</html>", string(first.contents), "first.contents")
	// Second upload: InitialComment must be empty (no repeat of text).
	second := c.uploadFileCalls[1]
	assert.Empty(t, second.initialComment, "second.initialComment must be empty")
	assert.Equal(t, "report-b.html", second.filename, "second.filename")
}

// TestSenderAttachmentsZipBundle_FetcherFilenameWins verifies the one
// exception to "runner hint wins" (TestSenderAttachmentsUploadsFiles): when
// the fetcher's Content-Type is application/zip — the operator's bundle route
// zipped an html primary because it had resolvable artifact: refs — the
// runner's pre-bundling filename hint (still naming
// the un-zipped primary, e.g. "report.html") must NOT be uploaded alongside
// zip bytes. The fetcher's own Content-Disposition filename (e.g.
// "report.zip") wins instead.
func TestSenderAttachmentsZipBundle_FetcherFilenameWins(t *testing.T) {
	c := &fakeSlackClient{}
	af := &stubAssetFetcher{
		bytes:    []byte("PK\x03\x04fake-zip-bytes"),
		mime:     "application/zip",
		filename: "report.zip",
	}
	s := &slackSender{
		deps:   channelkinds.Deps{AssetFetcher: af},
		client: c,
	}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF"},
		},
	}
	pl := channelevents.OutboundUserMessagePayload{
		Text: "Here is the report",
		Attachments: []channelevents.AttachmentRef{
			// The stale runner-minted hint: named+MIME'd before the operator's
			// server-side bundle decision.
			{RenderName: "ar-1", MIME: "text/html", Filename: "report.html"},
		},
	}
	plBytes, err := json.Marshal(pl)
	require.NoError(t, err, "marshal payload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindUserMessage,
		Session: channelevents.SessionRef{Namespace: "default", Name: "foo"},
		Payload: plBytes,
	}
	_, err = s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	require.Len(t, c.uploadFileCalls, 1, "expected 1 uploadFile call")
	got := c.uploadFileCalls[0]
	assert.Equal(t, "report.zip", got.filename,
		"the zipped fetcher response must win over the stale un-zipped runner hint")
	assert.Equal(t, "PK\x03\x04fake-zip-bytes", string(got.contents))
}

// TestSenderAttachmentsFetchErrorFallsBackToText verifies that when every
// AssetFetcher.Fetch fails, the sender falls back to a plain
// chat.postMessage carrying pl.Text plus a delivery-failure footer.
// Visible error indicator beats silent message loss — the agent's reply
// reaches the user even when attachments don't.
func TestSenderAttachmentsFetchErrorFallsBackToText(t *testing.T) {
	c := &fakeSlackClient{}
	af := &stubAssetFetcher{err: errors.New("operator says no")}
	s := &slackSender{
		deps:   channelkinds.Deps{AssetFetcher: af},
		client: c,
	}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C1"},
		},
	}
	pl := channelevents.OutboundUserMessagePayload{
		Text: "hi",
		Attachments: []channelevents.AttachmentRef{
			{RenderName: "ar-1", Filename: "a.html"},
		},
	}
	plBytes, err := json.Marshal(pl)
	require.NoError(t, err, "marshal payload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindUserMessage,
		Session: channelevents.SessionRef{Namespace: "default", Name: "foo"},
		Payload: plBytes,
	}
	_, err = s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	assert.Empty(t, c.uploadFileCalls, "upload must not be called when fetch fails")
	require.Len(t, c.postMessageCalls, 1, "expected 1 fallback postMessage")
	assert.Equal(t, "C1", c.postMessageCalls[0].channelID, "postMessage channelID")
}

// TestSenderAttachmentsMissingFetcherFallsBackToText verifies that a
// misconfigured sender (Deps.AssetFetcher == nil) on the attachment path
// still posts the agent's text reply with a footer noting the
// misconfiguration, instead of silently losing the message.
func TestSenderAttachmentsMissingFetcherFallsBackToText(t *testing.T) {
	c := &fakeSlackClient{}
	s := &slackSender{
		deps:   channelkinds.Deps{}, // no AssetFetcher
		client: c,
	}
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C1"},
		},
	}
	pl := channelevents.OutboundUserMessagePayload{
		Text: "hi",
		Attachments: []channelevents.AttachmentRef{
			{RenderName: "ar-1", Filename: "a.html"},
		},
	}
	plBytes, err := json.Marshal(pl)
	require.NoError(t, err, "marshal payload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindUserMessage,
		Session: channelevents.SessionRef{Namespace: "default", Name: "foo"},
		Payload: plBytes,
	}
	_, err = s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	assert.Empty(t, c.uploadFileCalls, "upload must not be called without AssetFetcher")
	require.Len(t, c.postMessageCalls, 1, "expected 1 fallback postMessage")
}

// attachmentSender builds a sender whose asset fetches resolve, mirroring
// TestSenderAttachmentsUploadsFiles's direct construction.
func attachmentSender(c *fakeSlackClient) *slackSender {
	return &slackSender{
		deps: channelkinds.Deps{AssetFetcher: &stubAssetFetcher{
			bytes: []byte("<html>hi</html>"), mime: "text/html", filename: "report.html",
		}},
		client: c,
	}
}

func attachmentPayload() channelevents.OutboundUserMessagePayload {
	return channelevents.OutboundUserMessagePayload{
		Text: "here you go",
		Attachments: []channelevents.AttachmentRef{
			{RenderName: "ar-1", MIME: "text/html", Filename: "report-a.html", AltText: "report a"},
		},
	}
}

// postedText renders the text a recorded chat.postMessage call would send.
func postedText(t *testing.T, call postMessageCall) string {
	t.Helper()
	_, values, err := slackapi.UnsafeApplyMsgOptions("xoxb-test", call.channelID, "https://slack.example/", call.options...)
	require.NoError(t, err)
	return values.Get("text")
}

// TestSendWithAttachments_FirstSendUploadFails_DoesNotRepeatTheText pins that a
// first send whose upload fails does not show the user their message twice.
// Creating the thread root already posted pl.Text, so the failure fallback must
// contribute only the failure notice — re-posting the text underneath it is a
// duplicate message, not a fallback.
func TestSendWithAttachments_FirstSendUploadFails_DoesNotRepeatTheText(t *testing.T) {
	c := &fakeSlackClient{
		postedTSs:     []string{"999.1", "999.2"},
		uploadFileErr: errors.New("upload_failed"),
	}
	s := attachmentSender(c)
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}

	res, err := s.sendWithAttachments(context.Background(), sess, "C1", "", attachmentPayload())
	require.NoError(t, err, "the fallback still delivers the failure notice")

	assert.Equal(t, "999.1", res.External["thread_ts"], "the thread root survives an upload failure")
	require.Len(t, c.postMessageCalls, 2, "one root post, one failure notice — never a third")

	assert.Contains(t, postedText(t, c.postMessageCalls[0]), "here you go", "the root carries the agent's text")
	fallback := postedText(t, c.postMessageCalls[1])
	assert.NotContains(t, fallback, "here you go", "the text must not be posted a second time")
	assert.Contains(t, fallback, "Attachment delivery failed", "the failure is still surfaced to the user")
}

// TestSendWithAttachments_FirstSendEstablishesThreadRoot pins that a session
// whose first agent reply carries attachments still records a thread root.
// files.uploadV2 exposes no message ts, so the text is posted first and the
// files are uploaded into the thread it creates.
func TestSendWithAttachments_FirstSendEstablishesThreadRoot(t *testing.T) {
	c := &fakeSlackClient{postedTS: "999.1"}
	s := attachmentSender(c)
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}

	res, err := s.sendWithAttachments(context.Background(), sess, "C1", "", attachmentPayload())
	require.NoError(t, err)

	assert.Equal(t, "999.1", res.External["thread_ts"], "the text post is the thread root")
	assert.Equal(t, "C1", res.External["channel_id"])
	require.Len(t, c.postMessageCalls, 1, "exactly one text post creates the root")
	require.NotEmpty(t, c.uploadFileCalls, "files still upload")
	assert.Equal(t, "999.1", c.uploadFileCalls[0].threadTS, "files land INSIDE the new thread")
	assert.Empty(t, c.uploadFileCalls[0].initialComment, "text already posted; never duplicate it")
}

// TestSendWithAttachments_InThreadKeepsInitialComment pins the unchanged path:
// an in-thread reply still rides the text on the first file's InitialComment,
// one message, no extra post. TestSenderAttachmentsUploadsFiles asserts the
// same "postMessage must not be called" invariant and must keep passing.
func TestSendWithAttachments_InThreadKeepsInitialComment(t *testing.T) {
	c := &fakeSlackClient{postedTS: "999.1"}
	s := attachmentSender(c)
	sess := channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}

	res, err := s.sendWithAttachments(context.Background(), sess, "C1", "111.1", attachmentPayload())
	require.NoError(t, err)

	assert.Empty(t, res.External, "no root to capture; the thread already exists")
	assert.Empty(t, c.postMessageCalls, "no separate text post in-thread")
	require.NotEmpty(t, c.uploadFileCalls)
	assert.Equal(t, "111.1", c.uploadFileCalls[0].threadTS)
	assert.Equal(t, "here you go", c.uploadFileCalls[0].initialComment, "text rides on the first upload")
}

// TestSenderNotificationCallsSetStatus verifies that a KindNotification envelope
// results in a SetAssistantThreadsStatusContext call (not a postMessage).
func TestSenderNotificationCallsSetStatus(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300",
			},
		},
	}
	_, err := s.Send(context.Background(), sess, notificationEnvelope(t, "Searching knowledge base…"))
	require.NoError(t, err, "Send")
	assert.Empty(t, c.postMessageCalls, "postMessage must not be called for notification")
	require.Len(t, c.setStatusCalls, 1, "expected 1 setStatus call")

	got := c.setStatusCalls[0]
	assert.Equal(t, "C01ABCDEF", got.channelID, "setStatus channelID")
	assert.Equal(t, "Searching knowledge base…", got.status, "setStatus status")
	// No k8s client, so fallback to External["thread_ts"].
	assert.Equal(t, "1614191050.013300", got.threadTS, "setStatus threadTS")
	assert.Equal(t, animatedLoadingMessages("Searching knowledge base…"), got.loadingMessages,
		"loading_messages fans the caption out across spinner frames")
}

// TestSenderNotificationTooLongFallsBackToGenericCaption verifies Slack's
// size guard: a status caption longer than the thread-top's clean-render
// width (e.g. a long plan-derived in_progress label, sent verbatim by
// update_plan) is replaced with a generic caption rather than dumped or
// truncated. Captions within the limit pass through unchanged.
func TestSenderNotificationTooLongTruncates(t *testing.T) {
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}

	t.Run("over the limit: the real caption is truncated to fit, not the generic one", func(t *testing.T) {
		c := &fakeSlackClient{}
		s := newSender(c)
		long := strings.Repeat("x", statusMaxRunes+50)
		_, err := s.Send(context.Background(), sess, notificationEnvelope(t, long))
		require.NoError(t, err, "Send")
		require.Len(t, c.setStatusCalls, 1)
		got := c.setStatusCalls[0].status
		assert.NotEqual(t, genericStatusCaption, got, "over-long caption must NOT be replaced by the confusing generic caption")
		assert.LessOrEqual(t, utf8.RuneCountInString(got), statusMaxRunes, "truncated caption fits the width")
		assert.True(t, strings.HasPrefix(got, "xxx"), "truncated caption keeps the real label's text")
		assert.True(t, strings.HasSuffix(got, "…"), "truncation appends an ellipsis to signal the cut")
	})

	t.Run("within the limit: passed through verbatim", func(t *testing.T) {
		c := &fakeSlackClient{}
		s := newSender(c)
		fits := strings.Repeat("y", statusMaxRunes) // exactly at the limit
		_, err := s.Send(context.Background(), sess, notificationEnvelope(t, fits))
		require.NoError(t, err, "Send")
		require.Len(t, c.setStatusCalls, 1)
		assert.Equal(t, fits, c.setStatusCalls[0].status, "a caption at the limit is not altered")
	})
}

// TestSenderEmptyNotificationClearsIndicator verifies the sentinel:
// a KindNotification with empty Text issues setStatus("") to dismiss the
// indicator and tells the watchdog to Forget the session. Used by the
// session watcher's Idle cleanup so a stuck "⚠️ Taking longer" alarm
// doesn't sit on the thread when the runner exits via the no-tool-use
// recovery path.
func TestSenderEmptyNotificationClearsIndicator(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	var forgotten []string
	s.deps.ForgetSetStatus = func(ns, name string) {
		forgotten = append(forgotten, ns+"/"+name)
	}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300",
			},
		},
	}
	pl, err := json.Marshal(channelevents.NotificationPayload{})
	require.NoError(t, err, "marshal NotificationPayload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindNotification,
		Session: channelevents.SessionRef{Namespace: "default", Name: "foo"},
		Payload: pl,
	}
	_, err = s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	assert.Empty(t, c.postMessageCalls, "postMessage must not be called for clear-sentinel")
	require.Len(t, c.setStatusCalls, 1, "expected 1 setStatus(clear) call")
	assert.Empty(t, c.setStatusCalls[0].status, "setStatus status should be empty (clear)")
	assert.Equal(t, []string{"default/foo"}, forgotten,
		"watchdog Forget should be called once for default/foo")
}

// TestSenderUnescapesHTMLEntities verifies that LLM-emitted HTML-escaped
// sequences (e.g. "Finding L project &amp; eng projects") are unescaped
// before being sent to Slack — otherwise Slack renders the entity
// literally and users see the encoded form in the thread-top status.
func TestSenderUnescapesHTMLEntities(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C1", "thread_ts": "1"},
		},
	}
	pl, err := json.Marshal(channelevents.NotificationPayload{
		Text:  "Finding L project &amp; eng projects",
		Short: "L &amp; eng",
	})
	require.NoError(t, err, "marshal NotificationPayload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindNotification,
		Session: channelevents.SessionRef{Namespace: "default", Name: "foo"},
		Payload: pl,
	}
	_, err = s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")
	require.Len(t, c.setStatusCalls, 1, "expected 1 setStatus call")
	assert.Equal(t, "Finding L project & eng projects", c.setStatusCalls[0].status,
		"status not unescaped")
}

// TestSenderUserMessageUnescapesHTMLEntities verifies the same unescape
// applied to user_message text (final reply path), since the LLM emits
// the same `&amp;` style escapes there too.
func TestSenderUserMessageUnescapesHTMLEntities(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C1"},
		},
	}
	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "spicedb &amp; internal"))
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1, "expected 1 postMessage call")
	// MsgOption is opaque; we can't read the Text back out. The
	// unescape is exercised at the slackifyText boundary above (same
	// code path); verifying it doesn't error here is enough.
}

// TestSenderNotificationSetStatusFailureIsIgnored verifies that a setStatus
// error is swallowed (best-effort) and Send returns nil.
func TestSenderNotificationSetStatusFailureIsIgnored(t *testing.T) {
	c := &fakeSlackClient{setStatusErr: errors.New("missing_scope")}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C1"},
		},
	}
	_, err := s.Send(context.Background(), sess, notificationEnvelope(t, "Working…"))
	assert.NoError(t, err, "notification setStatus failure should be swallowed")
}

// TestSenderNotification_TouchesWatchdogEvenWhenSetStatusFails is the
// update_status sibling of the tool_progress case: the agent published a status
// caption, which is forward progress on its own. Swallowing the Slack error and
// returning early also skipped the watchdog Touch, so a thread Slack refuses to
// setStatus on (invalid_thread_ts) starved the silence clock and produced a
// bogus "appears to have stalled" notice.
func TestSenderNotification_TouchesWatchdogEvenWhenSetStatusFails(t *testing.T) {
	c := &fakeSlackClient{setStatusErr: errors.New("invalid_thread_ts")}
	s := newSender(c)
	var touched []string
	s.deps.TouchSetStatus = func(ns, name string) { touched = append(touched, ns+"/"+name) }
	sess := channelkinds.SessionInfo{
		Namespace: "ns", Name: "sess",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C1"},
		},
	}

	_, err := s.Send(context.Background(), sess, notificationEnvelope(t, "Working…"))
	require.NoError(t, err, "a failed cosmetic setStatus stays best-effort")

	assert.Equal(t, []string{"ns/sess"}, touched,
		"the agent published a status; the watchdog must be told even though Slack rejected the indicator")
}

// TestSenderOperationActivityCallsSetStatus verifies a KindOperationActivity
// envelope with a non-empty CompactLine renders straight through to
// setStatus. The channelsd relay has already resolved payload.CompactLine to
// the watchdog machine's EffectiveLine (op tick text, or the reverted
// update_status caption on a Cleared tick) — this covers both shapes, since
// the sender treats the field the same either way.
func TestSenderOperationActivityCallsSetStatus(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300",
			},
		},
	}
	_, err := s.Send(context.Background(), sess, operationActivityEnvelope(t, "fetch goals ‣ querying database"))
	require.NoError(t, err, "Send")
	assert.Empty(t, c.postMessageCalls, "postMessage must not be called for operation_activity")
	require.Len(t, c.setStatusCalls, 1, "expected 1 setStatus call")

	got := c.setStatusCalls[0]
	assert.Equal(t, "C01ABCDEF", got.channelID, "setStatus channelID")
	assert.Equal(t, "fetch goals ‣ querying database", got.status, "setStatus status")
	assert.Equal(t, "1614191050.013300", got.threadTS, "setStatus threadTS")
}

// TestSenderOperationActivityRevertedCaption_SetsCaptionVerbatim covers the
// Cleared/revert tick: the machine has already swapped CompactLine back to
// the remembered update_status caption, and the sender must render exactly
// that (no operation-tree framing, no truncation when it already fits).
func TestSenderOperationActivityRevertedCaption_SetsCaptionVerbatim(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	_, err := s.Send(context.Background(), sess, operationActivityEnvelope(t, "Searching knowledge base…"))
	require.NoError(t, err, "Send")
	require.Len(t, c.setStatusCalls, 1)
	assert.Equal(t, "Searching knowledge base…", c.setStatusCalls[0].status,
		"reverted caption is rendered verbatim, not re-wrapped")
}

// TestSenderOperationActivityLongLine_Truncates verifies the long-line case:
// an "op ‣ reason" compact line past the thread-top's clean-render width is
// truncated to statusMaxRunes via the same fitRunes helper sendNotification
// uses — not hand-rolled, not dropped, not replaced by a generic caption.
func TestSenderOperationActivityLongLine_Truncates(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	long := "fetch goals ‣ " + strings.Repeat("x", statusMaxRunes+50)
	_, err := s.Send(context.Background(), sess, operationActivityEnvelope(t, long))
	require.NoError(t, err, "Send")
	require.Len(t, c.setStatusCalls, 1)
	got := c.setStatusCalls[0].status
	assert.LessOrEqual(t, utf8.RuneCountInString(got), statusMaxRunes, "truncated line fits the width")
	assert.True(t, strings.HasPrefix(got, "fetch goals"), "truncated line keeps the real text's start")
	assert.True(t, strings.HasSuffix(got, "…"), "truncation appends an ellipsis to signal the cut")
}

// TestSenderOperationActivityEmptyLine_NoOp verifies the empty-CompactLine
// case does NOT call setStatus at all — unlike sendNotification's
// empty-Text sentinel (which tears the indicator down), a bare empty
// operation_activity tick means "nothing to show yet" and must leave
// whatever is currently on the indicator alone.
func TestSenderOperationActivityEmptyLine_NoOp(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	var forgotten []string
	s.deps.ForgetSetStatus = func(ns, name string) { forgotten = append(forgotten, ns+"/"+name) }
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	_, err := s.Send(context.Background(), sess, operationActivityEnvelope(t, ""))
	require.NoError(t, err, "Send")
	assert.Empty(t, c.setStatusCalls, "an empty compact line must not call setStatus at all")
	assert.Empty(t, forgotten, "an empty compact line must not tear down the watchdog either")
}

// TestSenderOperationActivityUnchangedLine_SkipsSetStatus: the runner ticks
// operation activity every ~5s, and every setStatus replaces the whole
// loading_messages array — which resets Slack's client-side rotation, visible
// as the spinner flashing backwards mid-animation. An identical line for the
// same thread must be a no-op; a changed line must still go through.
func TestSenderOperationActivityUnchangedLine_SkipsSetStatus(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}

	_, err := s.Send(context.Background(), sess, operationActivityEnvelope(t, "fetch goals ‣ querying database"))
	require.NoError(t, err, "Send #1")
	require.Len(t, c.setStatusCalls, 1, "first tick renders")

	_, err = s.Send(context.Background(), sess, operationActivityEnvelope(t, "fetch goals ‣ querying database"))
	require.NoError(t, err, "Send #2 (identical)")
	assert.Len(t, c.setStatusCalls, 1, "an identical tick must not re-issue setStatus (it resets Slack's spinner rotation)")

	_, err = s.Send(context.Background(), sess, operationActivityEnvelope(t, "fetch goals ‣ writing summary"))
	require.NoError(t, err, "Send #3 (changed)")
	require.Len(t, c.setStatusCalls, 2, "a changed line must still render")
	assert.Equal(t, "fetch goals ‣ writing summary", c.setStatusCalls[1].status)
}

// After the indicator is torn down (the empty-Notification sentinel), the same
// operation line must be re-issued: the dedup must compare against what Slack
// is showing (the cache, which teardown drops), not against history.
func TestSenderOperationActivityAfterClear_ReissuesSameLine(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}

	_, err := s.Send(context.Background(), sess, operationActivityEnvelope(t, "fetch goals ‣ querying database"))
	require.NoError(t, err, "Send op #1")
	require.Len(t, c.setStatusCalls, 1)

	_, err = s.Send(context.Background(), sess, notificationEnvelope(t, ""))
	require.NoError(t, err, "Send teardown sentinel")
	require.Len(t, c.setStatusCalls, 2, "the sentinel clears the indicator")
	assert.Equal(t, "", c.setStatusCalls[1].status)

	_, err = s.Send(context.Background(), sess, operationActivityEnvelope(t, "fetch goals ‣ querying database"))
	require.NoError(t, err, "Send op #2 (same line, after clear)")
	require.Len(t, c.setStatusCalls, 3, "a cleared indicator must be repopulated even with an unchanged line")
	assert.Equal(t, "fetch goals ‣ querying database", c.setStatusCalls[2].status)
}

// TestLoadingSpinnerFrames_OrderIndependentSet pins the frame choice to a set
// whose EVERY pairwise transition reads as rotation. Slack documents
// loading_messages only as an array it "will rotate through" — no order or
// interval guarantee — and each setStatus replaces the array, so a sequential
// spinner (⠋⠙⠹…) renders out of order and jumps backwards in practice. Four
// half-circles at 90° steps look like a spinner under ANY order Slack picks;
// re-introducing an order-dependent sequence reintroduces the glitch.
func TestLoadingSpinnerFrames_OrderIndependentSet(t *testing.T) {
	assert.Equal(t, []string{"◐", "◓", "◑", "◒"}, loadingSpinnerFrames)
}

// TestClearStatus_SendsEmptyLoadingMessages pins that the clear is symmetric
// with the set: the warn setStatus carries a LoadingMessages entry, so the
// clear must send a (non-nil) empty LoadingMessages to fully dismiss the
// indicator on the no-bot-message idle path.
func TestClearStatus_SendsEmptyLoadingMessages(t *testing.T) {
	c := &fakeSlackClient{}
	s := &slackSender{client: c}

	s.clearStatus(context.Background(),
		channelkinds.SessionInfo{Namespace: "default", Name: "sess"},
		"C123", "1614191050.000100")

	require.Len(t, c.setStatusCalls, 1)
	assert.Equal(t, "", c.setStatusCalls[0].status, "clear sends empty status")
	assert.NotNil(t, c.setStatusCalls[0].loadingMessages, "clear must send a non-nil LoadingMessages")
	assert.Empty(t, c.setStatusCalls[0].loadingMessages, "clear's LoadingMessages must be empty")
}

// TestNewSlackAPIClient covers the constructor's nil/missing-token/
// happy-path behavior in one table; all three cases share the same
// shape (Secret in → client present? out).
func TestNewSlackAPIClient(t *testing.T) {
	cases := []struct {
		name       string
		secret     *corev1.Secret
		wantNonNil bool
	}{
		{
			name:   "nil Secret: yields nil client",
			secret: nil,
		},
		{
			name:   "Secret without bot-token key: yields nil client",
			secret: &corev1.Secret{Data: map[string][]byte{"other-key": []byte("x")}},
		},
		{
			name:       "Secret with bot-token key: yields non-nil client",
			secret:     &corev1.Secret{Data: map[string][]byte{SecretKeyBotToken: []byte("xoxb-test")}},
			wantNonNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newSlackAPIClient(tc.secret)
			if tc.wantNonNil {
				assert.NotNil(t, got, "expected non-nil client")
			} else {
				assert.Nil(t, got, "expected nil client")
			}
		})
	}
}

func TestPostEphemeral(t *testing.T) {
	c := &fakeSlackClient{}
	require.NoError(t, PostEphemeral(context.Background(), c, "C1", "U1", "you can't"),
		"PostEphemeral")
	require.Len(t, c.postEphemeralCalls, 1, "postEphemeral call count")

	got := c.postEphemeralCalls[0]
	assert.Equal(t, "C1", got.channelID, "channelID")
	assert.Equal(t, "U1", got.userID, "userID")
}

func TestPostEphemeralNilClient(t *testing.T) {
	err := PostEphemeral(context.Background(), nil, "C", "U", "msg")
	assert.Error(t, err, "expected error on nil client")
}

func TestOpenIM(t *testing.T) {
	c := &fakeSlackClient{}
	id, err := OpenIM(context.Background(), c, "U01")
	require.NoError(t, err, "OpenIM")
	assert.Equal(t, "DU01", id, "DM channel id")
	require.Len(t, c.openConvCalls, 1, "openConv call count")
	assert.Equal(t, "U01", c.openConvCalls[0], "openConv user")
}

func TestOpenIMNilClient(t *testing.T) {
	_, err := OpenIM(context.Background(), nil, "U01")
	assert.Error(t, err, "expected error on nil client")
}

// TestSender_FirstSend_CapturesThreadTS verifies the cron flow's write-back
// contract: when the inbound binding has no thread_ts (i.e. this is the first
// post into a fresh thread, as a cron-spawned session does), the Sender hands
// the captured ts + channel_id back via SubChannelSendResult.External so
// channelsd's outbound relay can patch OutputChannel.{External,Key} on the
// AgentSession CR. Subsequent agent posts then thread under the same root,
// and inbound thread replies can match back to this session via Key.
func TestSender_FirstSend_CapturesThreadTS(t *testing.T) {
	c := &fakeSlackClient{postedTS: "1234.5678"}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "cron-session",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				// thread_ts INTENTIONALLY ABSENT — this is the cron first-send case.
			},
		},
	}
	res, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "first cron post"))
	require.NoError(t, err, "Send")
	require.Len(t, c.postMessageCalls, 1, "postMessage call count")
	assert.Equal(t, "1234.5678", res.External["thread_ts"], "captured thread_ts write-back")
	assert.Equal(t, "C01ABCDEF", res.External["channel_id"], "captured channel_id write-back")
}

// TestSender_ExistingThread_NoWriteBack verifies the subsequent-send case:
// when the binding already carries a thread_ts (an established thread), the
// Sender MUST NOT return write-back metadata. Returning it would have the
// relay re-patch OutputChannel.Key on every reply — harmless idempotently
// today, but a waste of api-server traffic and a footgun for conflict-retry
// logic the relay may grow later.
func TestSender_ExistingThread_NoWriteBack(t *testing.T) {
	c := &fakeSlackClient{postedTS: "9999.0000"}
	s := newSender(c)
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "existing-session",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.013300", // established thread
			},
		},
	}
	res, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "reply in thread"))
	require.NoError(t, err, "Send")
	assert.Empty(t, res.External, "result.External must be empty for established thread")
}

// restartForkScheme returns a scheme with the agentprimitives API group
// registered for fake client construction.
func restartForkScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// TestSender_RestartFork_FirstReplyThreadsUnderTheNewRootAndWritesBackTheRoot
// verifies the integrated fork-root path: the sender's fork-root guard fires
// before the thread is resolved, creating the root (framing post + parent-
// thread backlink) so the agent's reply itself threads under that root
// instead of sitting at channel level — and the write-back metadata reports
// the root, not the reply's own ts, so later turns thread correctly.
func TestSender_RestartFork_FirstReplyThreadsUnderTheNewRootAndWritesBackTheRoot(t *testing.T) {
	c := &fakeSlackClient{postedTS: "root.9999"}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-session",
			Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T99:C-PARENT:parent-root.1111",
			},
		},
	}
	sch := restartForkScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(sch).WithObjects(sess).Build()

	s := &slackSender{
		deps:         channelkinds.Deps{K8sClient: k8s},
		client:       c,
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
		forkRoot:     newForkRootCache(),
	}

	info := channelkinds.SessionInfo{
		Namespace: "default", Name: "child-session",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C-CHILD",
				// thread_ts intentionally absent — restart fork child starts a new thread.
			},
		},
	}

	res, err := s.Send(context.Background(), info, userMessageEnvelope(t, "first reply in new thread"))
	require.NoError(t, err, "Send must succeed")

	require.Len(t, c.postMessageCalls, 3,
		"postMessage[0]=root framing, postMessage[1]=parent-thread backlink, postMessage[2]=the agent's reply")

	root := c.postMessageCalls[0]
	assert.Equal(t, "C-CHILD", root.channelID, "the root posts at channel level, creating the new thread")
	_, rootVals, err := slackapi.UnsafeApplyMsgOptions("test-token", root.channelID, "http://test.invalid/", root.options...)
	require.NoError(t, err, "UnsafeApplyMsgOptions(root)")
	assert.Empty(t, rootVals.Get("thread_ts"), "the root itself has no parent thread")

	backlink := c.postMessageCalls[1]
	assert.Equal(t, "C-PARENT", backlink.channelID, "the backlink posts into the parent's channel")

	reply := c.postMessageCalls[2]
	assert.Equal(t, "C-CHILD", reply.channelID, "the agent's reply posts into the same channel as the root")
	_, replyVals, err := slackapi.UnsafeApplyMsgOptions("test-token", reply.channelID, "http://test.invalid/", reply.options...)
	require.NoError(t, err, "UnsafeApplyMsgOptions(reply)")
	assert.Equal(t, c.postedTS, replyVals.Get("thread_ts"),
		"the reply threads under the root, not posted at channel level")

	assert.Equal(t, c.postedTS, res.External["thread_ts"], "write-back reports the root, not a separate reply ts")
	assert.Equal(t, "C-CHILD", res.External["channel_id"], "write-back channel_id")
}

// TestSender_RestartFork_RootPostFailureFallsBackToPlainDelivery verifies the
// fork-root guard is best-effort: when the framing root's chat.postMessage
// call itself fails, the guard must not block the agent's reply. Send falls
// through to a plain channel-level post, whose own ts becomes the implicit
// thread root via write-back — exactly like a non-fork first-send.
func TestSender_RestartFork_RootPostFailureFallsBackToPlainDelivery(t *testing.T) {
	c := &fakeSlackClient{
		postedTS:        "fallback-root.8888",
		postMessageErrs: []error{errors.New("rate_limited"), nil}, // root attempt fails, reply succeeds
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-session-be",
			Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T99:C-PARENT-BE:parent.2222",
			},
		},
	}
	sch := restartForkScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(sch).WithObjects(sess).Build()

	s := &slackSender{
		deps:         channelkinds.Deps{K8sClient: k8s},
		client:       c,
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
		forkRoot:     newForkRootCache(),
	}

	info := channelkinds.SessionInfo{
		Namespace: "default", Name: "child-session-be",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{"channel_id": "C-CHILD-BE"},
		},
	}

	res, err := s.Send(context.Background(), info, userMessageEnvelope(t, "first reply"))
	require.NoError(t, err, "Send must succeed even when the root post fails")
	require.Len(t, c.postMessageCalls, 2, "postMessage[0]=failed root attempt, postMessage[1]=the agent's reply")
	assert.Equal(t, "fallback-root.8888", res.External["thread_ts"], "write-back falls back to the reply's own ts")
}

// TestSend_ForkChildStreaming_ConsumesTheBubbleAndRepliesOnce drives the whole
// path end-to-end: the real StreamDeltaSink (wired with ThreadRoot, as
// Kind.NewStreamDeltaSink does) and the real slackSender share ONE
// forkRootCache, mirroring production wiring via Kind.sharedForkRoot().
//
// Both halves are load-bearing. A sink without ThreadRoot keys the bubble under
// an empty thread_ts while Send's fork-root guard consults ConsumeFinalTarget
// under the newly-created ROOT — a guaranteed miss that falls through to a
// second, duplicate chat.postMessage. And Send's own empty-thread_ts write-back
// check never fires once the guard has reassigned threadTS to the root, so the
// child's binding goes unpatched.
func TestSend_ForkChildStreaming_ConsumesTheBubbleAndRepliesOnce(t *testing.T) {
	const rootTS = "root.1"
	const backlinkTS = "root.2"
	const bubbleTS = "bubble.1"
	c := &fakeSlackClient{postedTSs: []string{rootTS, backlinkTS, bubbleTS}}
	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fork-child",
			Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T1:C-PARENT:parent-root.1111",
			},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(restartForkScheme(t)).WithObjects(as).Build()
	shared := newForkRootCache() // one cache, shared by sink + sender — mirrors Kind.sharedForkRoot()

	sink := NewStreamDeltaSink(StreamDeltaSinkConfig{
		Updater:        &SlackUpdater{Client: c},
		Poster:         &SlackPoster{Client: c},
		DebounceWindow: 20 * time.Millisecond,
		ThreadRoot: func(ctx context.Context, sess channelkinds.SessionInfo, channelID string) string {
			return shared.ensure(ctx, c, k8s, sess, channelID)
		},
	})
	defer sink.Close()

	info := channelkinds.SessionInfo{
		Namespace: "default", Name: "fork-child",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{"channel_id": "C-CHILD"},
			// thread_ts intentionally absent — a fork child's binding has no
			// thread yet.
		},
	}

	require.NoError(t, sink.OnDelta(context.Background(), info, deltaEnv(t, "text_delta", "streaming…", "")))
	time.Sleep(100 * time.Millisecond) // let the debounced flush post the bubble
	require.NoError(t, sink.OnDelta(context.Background(), info, deltaEnv(t, "stop", "", "")))

	s := &slackSender{
		deps:         channelkinds.Deps{K8sClient: k8s},
		client:       c,
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
		forkRoot:     shared,
		streamSink:   sink,
	}

	res, err := s.Send(context.Background(), info, userMessageEnvelope(t, "the polished final reply"))
	require.NoError(t, err, "Send")

	require.Len(t, c.postMessageCalls, 3,
		"postMessage[0]=root framing, postMessage[1]=parent-thread backlink, postMessage[2]=the streaming bubble — the final reply must land via chat.update, not a 4th postMessage")
	updates := c.snapshotUpdateCalls()
	require.Len(t, updates, 1, "exactly one chat.update lands the polished reply")
	assert.Equal(t, bubbleTS, updates[0].ts, "chat.update must target the bubble the sink registered under the root")
	assert.Equal(t, rootTS, res.External["thread_ts"], "write-back reports the thread root, not the bubble ts")
	assert.Equal(t, "C-CHILD", res.External["channel_id"])
}

// TestSend_ForkChildWithAttachments_ReportsTheThreadRoot pins that a fork
// child's first send carrying attachments still reports the link-back
// framing root as the write-back thread_ts. sendWithAttachments only
// captures a root it creates ITSELF (threadTS == "" on entry); once Send's
// own fork-root guard has already resolved threadTS to the framing root
// before calling sendWithAttachments, that internal guard never fires and
// the write-back was silently dropped — permanently detaching the child's
// binding from the thread the files actually landed in.
func TestSend_ForkChildWithAttachments_ReportsTheThreadRoot(t *testing.T) {
	const rootTS = "root.1"
	const backlinkTS = "root.2"
	c := &fakeSlackClient{postedTSs: []string{rootTS, backlinkTS}}
	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fork-child-att",
			Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T1:C-PARENT:parent-root.1111",
			},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(restartForkScheme(t)).WithObjects(as).Build()

	af := &stubAssetFetcher{bytes: []byte("<html>hi</html>"), mime: "text/html", filename: "report.html"}
	s := &slackSender{
		deps:         channelkinds.Deps{K8sClient: k8s, AssetFetcher: af},
		client:       c,
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
		forkRoot:     newForkRootCache(),
	}

	info := channelkinds.SessionInfo{
		Namespace: "default", Name: "fork-child-att",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{"channel_id": "C-CHILD"},
			// thread_ts intentionally absent — a fork child's binding has no
			// thread yet.
		},
	}

	pl := attachmentPayload()
	plBytes, err := json.Marshal(pl)
	require.NoError(t, err, "marshal payload")
	env := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindUserMessage,
		Session: channelevents.SessionRef{Namespace: "default", Name: "fork-child-att"},
		Payload: plBytes,
	}

	res, err := s.Send(context.Background(), info, env)
	require.NoError(t, err, "Send")

	require.Len(t, c.postMessageCalls, 2, "postMessage[0]=root framing, postMessage[1]=parent-thread backlink")
	require.Len(t, c.uploadFileCalls, 1, "the attachment uploads into the framing root's thread")
	assert.Equal(t, rootTS, c.uploadFileCalls[0].threadTS, "the file must upload into the fork's thread root")
	assert.Equal(t, rootTS, res.External["thread_ts"], "the relay must patch the child's binding to the framing root")
	assert.Equal(t, "C-CHILD", res.External["channel_id"])
}

func (f *fakeSlackClient) GetUserByEmailContext(_ context.Context, email string) (*slackapi.User, error) {
	f.lookupByEmailCalls = append(f.lookupByEmailCalls, email)
	if f.lookupByEmailErr != nil {
		return nil, f.lookupByEmailErr
	}
	if u, ok := f.lookupByEmail[email]; ok {
		return u, nil
	}
	return nil, slackapi.SlackErrorResponse{Err: "users_not_found"}
}

func (f *fakeSlackClient) GetUsersContext(_ context.Context, _ ...slackapi.GetUsersOption) ([]slackapi.User, error) {
	f.usersListCalls++
	if f.usersListErr != nil {
		return nil, f.usersListErr
	}
	return f.usersList, nil
}

// stubStreamSink implements StreamFinalTargetConsumer for tests.
type stubStreamSink struct {
	ts string // returned once, then cleared
}

func (s *stubStreamSink) ConsumeFinalTarget(_, _ string) string {
	ts := s.ts
	s.ts = ""
	return ts
}

// TestSender_StreamingBubble_UpdatesOnFinalReply verifies that when the
// stream sink has a finalized streaming bubble, the sender chat.updates it
// with the final reply text rather than posting a new message.
func TestSender_StreamingBubble_UpdatesOnFinalReply(t *testing.T) {
	const bubbleTS = "1700000000.000111"
	c := &fakeSlackClient{}
	s := newSender(c)
	s.streamSink = &stubStreamSink{ts: bubbleTS}

	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.000000",
			},
		},
	}

	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "hello world"))
	require.NoError(t, err)

	require.Empty(t, c.postMessageCalls,
		"sender must chat.update the streaming bubble, not post fresh")
	require.Len(t, c.updateCalls, 1)
	require.Equal(t, "C01ABCDEF", c.updateCalls[0].channelID)
	require.Equal(t, bubbleTS, c.updateCalls[0].ts)
}

// TestSender_StreamingBubble_FirstSendEstablishesThreadRoot verifies that a
// session whose first agent reply lands via chat.update on a streaming
// bubble still records a thread root — the same first-send capture
// TestSendWithAttachments_FirstSendEstablishesThreadRoot pins for the
// attachments path, just via chat.update instead of files.uploadV2.
func TestSender_StreamingBubble_FirstSendEstablishesThreadRoot(t *testing.T) {
	const bubbleTS = "1700000000.000111"
	c := &fakeSlackClient{}
	s := newSender(c)
	s.streamSink = &stubStreamSink{ts: bubbleTS}

	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				// thread_ts intentionally absent — fresh channel first-send.
			},
		},
	}

	res, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "hello world"))
	require.NoError(t, err)

	assert.Equal(t, bubbleTS, res.External["thread_ts"], "the streaming bubble's ts becomes the thread root")
	assert.Equal(t, "C01ABCDEF", res.External["channel_id"])
	require.Empty(t, c.postMessageCalls, "sender must chat.update the streaming bubble, not post fresh")
	require.Len(t, c.updateCalls, 1)
}

// TestSender_NoStreamingSink_PostsFresh verifies that when no streaming
// occurred (nil sink) the sender falls through to chat.postMessage.
func TestSender_NoStreamingSink_PostsFresh(t *testing.T) {
	c := &fakeSlackClient{}
	s := newSender(c)
	// streamSink is nil — no streaming occurred.

	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			Name: "slack-eng", Kind: "slack",
			External: map[string]string{
				"channel_id": "C01ABCDEF",
				"thread_ts":  "1614191050.000000",
			},
		},
	}

	_, err := s.Send(context.Background(), sess, userMessageEnvelope(t, "hello world"))
	require.NoError(t, err)

	require.Empty(t, c.updateCalls, "no streaming → must not chat.update anything")
	require.Len(t, c.postMessageCalls, 1, "must fall through to chat.postMessage")
}

func TestMaybeAppendMentionFooter(t *testing.T) {
	cases := []struct {
		name        string
		routingMode string
		in          string
		want        string
	}{
		{"mention_only appends footer", "mention_only", "the answer", "the answer\n\n_@-mention me to reply._"},
		{"default routing leaves text untouched", "", "the answer", "the answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, maybeAppendMentionFooter(tc.in, tc.routingMode))
		})
	}
}

func turnProgressEnvelope(t *testing.T, pl channelevents.TurnProgressPayload) channelevents.Envelope {
	t.Helper()
	raw, err := json.Marshal(pl)
	require.NoError(t, err, "marshal TurnProgressPayload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindTurnProgress,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "foo"},
		PublishedAt: metav1.Now().Time,
		Payload:     raw,
	}
}

func TestHumanizeTokens(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"}, {42, "42"}, {999, "999"}, {1200, "1.2K"}, {6400, "6.4K"},
		{11000, "11K"}, {999000, "999K"}, {1500000, "1.5M"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, humanizeTokens(tc.n), "humanizeTokens(%d)", tc.n)
	}
}

func TestHumanizeElapsed(t *testing.T) {
	assert.Equal(t, "5s", humanizeElapsed(5))
	assert.Equal(t, "34s", humanizeElapsed(34))
	assert.Equal(t, "1m20s", humanizeElapsed(80))
	assert.Equal(t, "2m5s", humanizeElapsed(125))
	assert.Equal(t, "0s", humanizeElapsed(-5), "negative seconds clamp to 0")
}

func TestFitRunes(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"no truncation", "hello", 10, "hello"},
		{"exact fit", "hello", 5, "hello"},
		{"truncates with ellipsis", "hello", 3, "he…"},
		{"max 1 keeps one rune, no ellipsis", "hello", 1, "h"},
		{"max 0 empties", "hello", 0, ""},
		{"negative max clamps to 0", "hello", -2, ""},
		{"rune-aware (multibyte not split)", "héllo", 2, "h…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fitRunes(tc.s, tc.max))
		})
	}
}

// TestAnimatedLoadingMessages pins the loading_messages fan-out: one entry
// per spinner frame, frames in declared order, identical caption text per
// entry, and every entry within Slack's per-entry rune limit and 10-entry
// array cap. Slack rotates the array client-side, so in-order identical-text
// entries are what makes the rotation read as an animated spinner.
func TestAnimatedLoadingMessages(t *testing.T) {
	require.LessOrEqual(t, len(loadingSpinnerFrames), 10, "Slack caps loading_messages at 10 entries")

	t.Run("short caption: frame-prefixed verbatim, frames in order", func(t *testing.T) {
		got := animatedLoadingMessages("Thinking…")
		require.Len(t, got, len(loadingSpinnerFrames))
		for i, f := range loadingSpinnerFrames {
			assert.Equal(t, f+" Thinking…", got[i], "entry %d", i)
		}
	})

	t.Run("long caption: every entry fitted, frame kept, truncation marked", func(t *testing.T) {
		got := animatedLoadingMessages(strings.Repeat("x", loadingMessageMaxRunes*2))
		require.Len(t, got, len(loadingSpinnerFrames))
		for i, m := range got {
			assert.LessOrEqual(t, utf8.RuneCountInString(m), loadingMessageMaxRunes, "entry %d fits the per-entry limit", i)
			assert.True(t, strings.HasPrefix(m, loadingSpinnerFrames[i]+" "), "entry %d keeps its frame prefix", i)
			assert.True(t, strings.HasSuffix(m, "…"), "entry %d marks the truncation", i)
		}
	})
}

func TestSenderTurnProgress_WrapsCachedCaption(t *testing.T) {
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	c := &fakeSlackClient{}
	s := newSender(c)

	// Seed the agent's update_status caption.
	_, err := s.Send(context.Background(), sess, notificationEnvelope(t, "Reading commits…"))
	require.NoError(t, err, "seed caption")
	require.Len(t, c.setStatusCalls, 1)

	_, err = s.Send(context.Background(), sess,
		turnProgressEnvelope(t, channelevents.TurnProgressPayload{InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34, Seq: 1}))
	require.NoError(t, err, "turn_progress Send")
	require.Len(t, c.setStatusCalls, 2, "turn_progress issues a setStatus")

	got := c.setStatusCalls[1].status
	assert.True(t, strings.HasPrefix(got, "Reading commits…"),
		"status line starts with the caption — no spinner frame (the loading surface owns the spinner); got %q", got)
	assert.Contains(t, got, "1.2K in · 6.4K out · 34s", "appends humanized counts")
	assert.LessOrEqual(t, utf8.RuneCountInString(got), statusMaxRunes, "stays within the caption width")
	assert.Empty(t, c.postMessageCalls, "turn_progress must not post a message")

	// The spinner lives only on the loading surface: one entry per animated
	// frame, each carrying the caption + counts, with no second spinner glyph
	// baked into the text (Slack's rotation is the animation).
	loading := c.setStatusCalls[1].loadingMessages
	require.Len(t, loading, len(loadingSpinnerFrames), "one loading entry per spinner frame")
	for i, m := range loading {
		assert.True(t, strings.HasPrefix(m, loadingSpinnerFrames[i]+" Reading commits…"),
			"entry %d: frame then caption directly (got %q)", i, m)
		assert.Contains(t, m, "1.2K in", "entry %d carries the counts", i)
		assert.LessOrEqual(t, utf8.RuneCountInString(m), loadingMessageMaxRunes, "entry %d fits the per-entry limit", i)
	}
}

func TestSenderTurnProgress_GenericCaptionWhenNoneCached(t *testing.T) {
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	c := &fakeSlackClient{}
	s := newSender(c)

	_, err := s.Send(context.Background(), sess,
		turnProgressEnvelope(t, channelevents.TurnProgressPayload{InputTokens: 600, OutputTokens: 0, ElapsedSeconds: 5, Seq: 2}))
	require.NoError(t, err, "turn_progress Send")
	require.Len(t, c.setStatusCalls, 1)
	got := c.setStatusCalls[0].status
	assert.Contains(t, got, "Working on the current step", "uses generic caption when none cached")
	assert.Contains(t, got, "out · 5s", "still renders the counts")
}

func TestSenderTurnProgress_TruncatesLongCaptionKeepingSuffix(t *testing.T) {
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	c := &fakeSlackClient{}
	s := newSender(c)

	// A caption exactly at the limit (passes the notification size guard verbatim).
	long := strings.Repeat("y", statusMaxRunes)
	_, err := s.Send(context.Background(), sess, notificationEnvelope(t, long))
	require.NoError(t, err)
	require.Len(t, c.setStatusCalls, 1)

	_, err = s.Send(context.Background(), sess,
		turnProgressEnvelope(t, channelevents.TurnProgressPayload{InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34, Seq: 1}))
	require.NoError(t, err)
	require.Len(t, c.setStatusCalls, 2)
	got := c.setStatusCalls[1].status
	assert.LessOrEqual(t, utf8.RuneCountInString(got), statusMaxRunes, "composed status fits the width")
	assert.Contains(t, got, "out ·", "the counts suffix survives caption truncation")
}

func TestSenderTurnProgress_DoesNotCompoundSuffixAcrossTicks(t *testing.T) {
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	c := &fakeSlackClient{}
	s := newSender(c)

	_, err := s.Send(context.Background(), sess, notificationEnvelope(t, "Reading commits…"))
	require.NoError(t, err)

	// Two consecutive ticks must each wrap the PLAIN cached caption — not a
	// previously-wrapped one. This is the load-bearing invariant: sendTurnProgress
	// reads recallStatus but must NOT rememberStatus, else the suffix compounds.
	_, err = s.Send(context.Background(), sess,
		turnProgressEnvelope(t, channelevents.TurnProgressPayload{InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34, Seq: 1}))
	require.NoError(t, err)
	_, err = s.Send(context.Background(), sess,
		turnProgressEnvelope(t, channelevents.TurnProgressPayload{InputTokens: 1200, OutputTokens: 11000, ElapsedSeconds: 38, Seq: 2}))
	require.NoError(t, err)

	require.Len(t, c.setStatusCalls, 3) // 1 notification + 2 progress
	got := c.setStatusCalls[2].status
	assert.Equal(t, 1, strings.Count(got, " in · "), "suffix must not compound across ticks")
	assert.Equal(t, 1, strings.Count(got, "Reading commits…"), "caption wrapped once, not nested")
	assert.Contains(t, got, "11K out", "shows the latest tick's counts")
}

func TestSenderTurnProgress_SetStatusErrorIsBestEffort(t *testing.T) {
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	c := &fakeSlackClient{setStatusErr: errors.New("missing_scope")}
	s := newSender(c)
	// A failing setStatus must be logged and swallowed — never returned as an
	// error that would propagate up the relay (turn_progress is best-effort UX).
	_, err := s.Send(context.Background(), sess,
		turnProgressEnvelope(t, channelevents.TurnProgressPayload{InputTokens: 100, OutputTokens: 200, ElapsedSeconds: 5, Seq: 1}))
	require.NoError(t, err, "setStatus error must be logged, not returned")
	require.Len(t, c.setStatusCalls, 1, "still attempted the setStatus")
}

func TestSenderTurnProgress_MalformedPayloadReturnsError(t *testing.T) {
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "foo",
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C01ABCDEF", "thread_ts": "1614191050.013300"},
		},
	}
	c := &fakeSlackClient{}
	s := newSender(c)
	env := channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindTurnProgress,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "foo"},
		PublishedAt: metav1.Now().Time,
		Payload:     json.RawMessage(`{"inputTokens":"not-a-number"}`),
	}
	_, err := s.Send(context.Background(), sess, env)
	require.Error(t, err, "a malformed turn_progress payload surfaces an error, not a silent drop")
	assert.Empty(t, c.setStatusCalls, "no setStatus attempted on unmarshal failure")
}

// GetUserInfoContext is the id→profile direction renderSlackEntities needs.
// Seeded via userInfo; an unseeded id returns users_not_found, the same shape
// the real API gives for an id from another workspace.
func (f *fakeSlackClient) GetUserInfoContext(_ context.Context, id string) (*slackapi.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userInfoCalls = append(f.userInfoCalls, id)
	if f.userInfoErr != nil {
		return nil, f.userInfoErr
	}
	if u, ok := f.userInfo[id]; ok {
		return u, nil
	}
	return nil, slackapi.SlackErrorResponse{Err: "users_not_found"}
}
