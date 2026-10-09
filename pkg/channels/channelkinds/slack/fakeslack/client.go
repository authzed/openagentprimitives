// Package fakeslack is an in-memory Slack Web API simulator for tests. It
// models the pieces of Slack our channel kind depends on — most importantly the
// message/thread tree (ts ↔ thread_ts parent/reply relationships) — so tests
// can assert WHERE a reply lands (threaded child vs top-level sibling), not just
// that an option string contained "thread_ts". It satisfies the slack package's
// injected slackClient and historyClient interfaces by structural typing.
package fakeslack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	slackapi "github.com/slack-go/slack"
)

// Message is one simulated Slack message.
type Message struct {
	ChannelID string
	TS        string // unique, sortable
	ThreadTS  string // "" = top-level; else the root ts this reply hangs under
	Text      string
	SubType   string // "" normal; "file_share" for uploads
	UserID    string // "" = the bot
}

type Client struct {
	mu         sync.Mutex
	seq        int
	byChannel  map[string][]*Message     // ordered messages per channel
	ephemerals map[string][]*Message     // channel → ephemerals (not in the tree)
	status     map[string]string         // "channel|thread_ts" → status
	titles     map[string]string         // "channel|thread_ts" → title
	titleCalls int                       // count of SetAssistantThreadsTitleContext invocations
	users      map[string]*slackapi.User // userID → user (seed via SeedUser)
	usersEmail map[string]*slackapi.User // email → user

	// files + downloadSrv back inbound-attachment fixtures — see
	// files.go's SeedFile/GetFileInfoContext/serveDownload/Close. downloadSrv
	// is lazily started (nil until the first SeedFile call).
	files       map[string]*fileFixture
	downloadSrv *httptest.Server
}

func New() *Client {
	return &Client{
		byChannel:  map[string][]*Message{},
		ephemerals: map[string][]*Message{},
		status:     map[string]string{},
		titles:     map[string]string{},
		users:      map[string]*slackapi.User{},
		usersEmail: map[string]*slackapi.User{},
	}
}

func (c *Client) nextTS() string { c.seq++; return fmt.Sprintf("1700000000.%06d", c.seq) }

// decodeMsg pulls text + thread_ts out of opaque MsgOptions the way Slack's
// server would, using slack-go's UnsafeApplyMsgOptions.
func decodeMsg(channelID string, opts ...slackapi.MsgOption) (text, threadTS string) {
	_, vals, _ := slackapi.UnsafeApplyMsgOptions("token", channelID, "https://slack.test/api/", opts...)
	return vals.Get("text"), vals.Get("thread_ts")
}

func (c *Client) PostMessageContext(_ context.Context, channelID string, opts ...slackapi.MsgOption) (string, string, error) {
	text, threadTS := decodeMsg(channelID, opts...)
	c.mu.Lock()
	defer c.mu.Unlock()
	ts := c.nextTS()
	c.byChannel[channelID] = append(c.byChannel[channelID],
		&Message{ChannelID: channelID, TS: ts, ThreadTS: threadTS, Text: text})
	return channelID, ts, nil
}

func (c *Client) UpdateMessageContext(_ context.Context, channelID, ts string, opts ...slackapi.MsgOption) (string, string, string, error) {
	text, _ := decodeMsg(channelID, opts...)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.byChannel[channelID] {
		if m.TS == ts {
			m.Text = text
			return channelID, ts, text, nil
		}
	}
	return channelID, ts, text, fmt.Errorf("fakeslack: update: message %s not found in %s", ts, channelID)
}

func (c *Client) PostEphemeralContext(_ context.Context, channelID, userID string, opts ...slackapi.MsgOption) (string, error) {
	text, threadTS := decodeMsg(channelID, opts...)
	c.mu.Lock()
	defer c.mu.Unlock()
	ts := c.nextTS()
	c.ephemerals[channelID] = append(c.ephemerals[channelID],
		&Message{ChannelID: channelID, TS: ts, ThreadTS: threadTS, Text: text, UserID: userID})
	return ts, nil
}

func (c *Client) SetAssistantThreadsStatusContext(_ context.Context, p slackapi.AssistantThreadsSetStatusParameters) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status[p.ChannelID+"|"+p.ThreadTS] = p.Status
	return nil
}

func (c *Client) SetAssistantThreadsTitleContext(_ context.Context, p slackapi.AssistantThreadsSetTitleParameters) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.titleCalls++
	c.titles[p.ChannelID+"|"+p.ThreadTS] = p.Title
	return nil
}

// SetTitleCallCount returns the number of times SetAssistantThreadsTitleContext
// has been invoked. Tests use this to assert a redundancy-guard skip actually
// avoided calling the API (TitleFor alone can't distinguish "skipped" from
// "re-set to the same value").
func (c *Client) SetTitleCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.titleCalls
}

func (c *Client) GetConversationRepliesContext(_ context.Context, p *slackapi.GetConversationRepliesParameters) ([]slackapi.Message, bool, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []slackapi.Message
	for _, m := range c.byChannel[p.ChannelID] {
		if m.TS == p.Timestamp || m.ThreadTS == p.Timestamp {
			out = append(out, slackapi.Message{Msg: slackapi.Msg{
				Timestamp: m.TS, ThreadTimestamp: m.ThreadTS, Text: m.Text, User: m.UserID, SubType: m.SubType,
			}})
		}
	}
	return out, false, "", nil
}

func (c *Client) GetConversationHistoryContext(_ context.Context, p *slackapi.GetConversationHistoryParameters) (*slackapi.GetConversationHistoryResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := &slackapi.GetConversationHistoryResponse{}
	for _, m := range c.byChannel[p.ChannelID] {
		if m.ThreadTS == "" { // history returns top-level messages
			resp.Messages = append(resp.Messages, slackapi.Message{Msg: slackapi.Msg{
				Timestamp: m.TS, Text: m.Text, User: m.UserID,
			}})
		}
	}
	return resp, nil
}

// UploadFileContext models files.uploadV2's threaded file-share message. Note:
// slack-go v0.23's UploadFileParameters.Channel is a single channel ID (not a
// slice) — one call uploads to one channel.
func (c *Client) UploadFileContext(_ context.Context, p slackapi.UploadFileParameters) (*slackapi.FileSummary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Model the file share as a threaded message so attachment threading is testable.
	ts := c.nextTS()
	c.byChannel[p.Channel] = append(c.byChannel[p.Channel],
		&Message{ChannelID: p.Channel, TS: ts, ThreadTS: p.ThreadTimestamp, Text: p.InitialComment, SubType: "file_share"})
	return &slackapi.FileSummary{ID: "F" + ts}, nil
}

// --- user lookups (seed with SeedUser) ---

func (c *Client) SeedUser(u *slackapi.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users[u.ID] = u
	if u.Profile.Email != "" {
		c.usersEmail[u.Profile.Email] = u
	}
}
func (c *Client) GetUserInfoContext(_ context.Context, id string, _ ...slackapi.GetUserInfoOption) (*slackapi.User, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.users[id]; ok {
		return u, nil
	}
	return &slackapi.User{ID: id}, nil
}

// GetBotInfoContext satisfies the history reader's bots.info lookup. The fake
// has no bot directory, so it echoes the id back as the name — the same shape
// the real API returns for a bot with no configured display name, and what the
// caller would fall back to anyway.
func (c *Client) GetBotInfoContext(_ context.Context, p slackapi.GetBotInfoParameters) (*slackapi.Bot, error) {
	return &slackapi.Bot{ID: p.Bot, Name: p.Bot}, nil
}

func (c *Client) GetUserByEmailContext(_ context.Context, email string) (*slackapi.User, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.usersEmail[email]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("fakeslack: no user for email %q", email)
}
func (c *Client) GetUsersContext(_ context.Context, _ ...slackapi.GetUsersOption) ([]slackapi.User, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slackapi.User, 0, len(c.users))
	for _, u := range c.users {
		out = append(out, *u)
	}
	return out, nil
}

// --- remaining slackClient methods: minimal but valid ---

func (c *Client) OpenConversationContext(_ context.Context, p *slackapi.OpenConversationParameters) (*slackapi.Channel, bool, bool, error) {
	ch := &slackapi.Channel{}
	ch.ID = "D" + p.Users[0] // deterministic DM channel id
	return ch, false, false, nil
}
func (c *Client) OpenViewContext(_ context.Context, _ string, _ slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	return &slackapi.ViewResponse{}, nil
}
func (c *Client) GetPermalinkContext(_ context.Context, p *slackapi.PermalinkParameters) (string, error) {
	return fmt.Sprintf("https://slack.test/archives/%s/p%s", p.Channel, p.Ts), nil
}

// --- listenerAPIClient-only methods (pkg/channels/channelkinds/slack/listener_client.go) ---

// GrantedBotScopes must be kept in sync with
// channelkinds.ScopesFor(&Kind{}, channelfeatures.All()) — the union of
// scopes declared by Kind's FeatureSupport (pkg/channels/channelkinds/slack/features.go):
// the listener's Start reads the X-OAuth-Scopes response header off auth.test
// and patches the Channel's ScopesValid condition from whatever is (or isn't)
// granted there. Returning the full required set here keeps e2e scenarios
// from tripping a spurious ScopesValid=false — a real drift would only be a
// harmless best-effort status patch, not a functional failure, but there's no
// reason to manufacture one in the fake.
//
// Exported so TestFakeGrantsEveryRequiredScope in the slack package can assert
// the two lists have not drifted — a comment alone does not survive the next
// scope somebody adds.
const GrantedBotScopes = "app_mentions:read,chat:write,chat:write.customize,commands,channels:history,groups:history,im:history,im:write,users:read,users:read.email,assistant:write,files:write,files:read,channels:read,groups:read"

// BotUserID is the fixed bot identity AuthTestContext reports. Tests and
// scenario helpers can reference it instead of hardcoding "UBOT".
const BotUserID = "UBOT"

// AuthTestContext resolves the fake bot's own identity, as auth.test would.
func (c *Client) AuthTestContext(_ context.Context) (*slackapi.AuthTestResponse, error) {
	hdr := http.Header{}
	hdr.Set("X-OAuth-Scopes", GrantedBotScopes)
	return &slackapi.AuthTestResponse{
		URL:    "https://slack.test/",
		Team:   "T-test",
		TeamID: "T-test",
		User:   "agentprimitives-bot",
		UserID: BotUserID,
		Header: hdr,
	}, nil
}

// OpenView is the non-context views.open used by the "Restart from here"
// shortcut's modal + error-modal paths.
func (c *Client) OpenView(_ string, _ slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	return &slackapi.ViewResponse{}, nil
}

// PublishViewContext publishes the App Home tab.
func (c *Client) PublishViewContext(_ context.Context, _ slackapi.PublishViewContextRequest) (*slackapi.ViewResponse, error) {
	return &slackapi.ViewResponse{}, nil
}
