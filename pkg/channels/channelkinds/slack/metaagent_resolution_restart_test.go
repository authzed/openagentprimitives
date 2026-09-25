package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
)

// recordingPostMessageAPI is a *slackapi.Client backed by an httptest server
// that acks chat.postMessage and records the posted text alongside the path,
// so a test can tell a permanent in-thread post from an ephemeral one.
func recordingPostMessageAPI(t *testing.T) (*slackapi.Client, func() []postedMessage) {
	t.Helper()
	var mu sync.Mutex
	var posts []postedMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		posts = append(posts, postedMessage{path: r.URL.Path, text: r.FormValue("text")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.0","channel":"CHAN"}`))
	}))
	t.Cleanup(srv.Close)
	return slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/")), func() []postedMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]postedMessage(nil), posts...)
	}
}

type postedMessage struct {
	path string
	text string
}

// TestMetaagentColdStartResolution_SurvivesCacheMiss is the regression guard
// for a durability defect: a cold-start scope-approval block is EPHEMERAL —
// only the starter ever saw it — so the permanent in-thread resolution message
// is the sole channel-visible record of what was decided. It was posted only
// from an in-process cache with no durable twin, and the cache-miss branch bare
// returned, with no log. A channelsd restart between posting the block and the
// approver clicking therefore erased the record silently, even though authzd
// had already persisted the request at publish time and the sibling Show
// Details handler reads exactly that record.
func TestMetaagentColdStartResolution_SurvivesCacheMiss(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	require.NoError(t, metaagentaudit.RecordRequested(context.Background(), mem,
		memory.Scope{Kind: "session", ID: "ns/sess"}, metaagentaudit.Content{
			RequestID:   "req-777",
			Requester:   "U_BOB",
			RequestText: "widen scope to tracker",
			ColdStart:   true,
			CleanedTask: "summarize the tracker board",
			ComposerOutput: metaagentaudit.ComposerOutput{
				ApproverSummary: "adds read access to the tracker",
			},
		}))

	api, posted := recordingPostMessageAPI(t)
	l := &slackListener{
		deps: channelkinds.Deps{
			Memory:      mem,
			NATSPublish: func(string, []byte) error { return nil },
		},
		api:                   api,
		idents:                NewIdentityCache(8),
		installedTeamID:       "T1",
		metaagentApprovalRefs: newMetaagentApprovalRefCache(), // empty: channelsd restarted
	}
	l.idents.Put(userInfo{UserID: "U_APPROVER", Email: "approver@example.com", TeamID: "T1"})

	cb := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		User:      slackapi.User{ID: "U_APPROVER"},
		Container: slackapi.Container{ChannelID: "CHAN", ThreadTs: "1700000000.000100"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{
			ActionID: "metaagent_approve_cleaned_req-777",
			Value:    MetaagentApprovalButtonValue("req-777", "ns/sess", MetaagentDecisionApproveCleaned),
		}}},
	}
	require.NoError(t,
		l.onInteraction(memory.WithSystemApproval(context.Background(), "test"), cb),
		"onInteraction")

	var permanent []postedMessage
	for _, p := range posted() {
		if strings.HasSuffix(p.path, "/chat.postMessage") {
			permanent = append(permanent, p)
		}
	}
	require.Len(t, permanent, 1,
		"the decision must leave a permanent in-thread record even when the "+
			"in-process cache is empty — the durable request record is right there")
	assert.Contains(t, permanent[0].text, "approved", "the record must state the decision")
	assert.Contains(t, permanent[0].text, "widen scope to tracker",
		"the record must quote what was asked for")
	assert.Contains(t, permanent[0].text, "summarize the tracker board",
		"the record must name the task that will now run")
}

// namingPostMessageAPI is recordingPostMessageAPI plus a real users.info
// answer, so a test can tell "the mention was decoded" from "every call
// returned an empty user and it degraded to the id".
//
// The two paths deliberately answer DIFFERENTLY. A fake that returns one
// blanket `{"ok":true}` makes users.info succeed with a nameless user, which
// looks exactly like a lookup that was never wired up.
func namingPostMessageAPI(t *testing.T, names map[string]string) (*slackapi.Client, func() []postedMessage) {
	t.Helper()
	var mu sync.Mutex
	var posts []postedMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/users.info") {
			id := r.FormValue("user")
			name, ok := names[id]
			if !ok {
				_, _ = w.Write([]byte(`{"ok":false,"error":"user_not_found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"` + id + `","real_name":"` + name + `"}}`))
			return
		}
		mu.Lock()
		posts = append(posts, postedMessage{path: r.URL.Path, text: r.FormValue("text")})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.0","channel":"CHAN"}`))
	}))
	t.Cleanup(srv.Close)
	return slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/")), func() []postedMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]postedMessage(nil), posts...)
	}
}

// TestMetaagentColdStartResolution_CacheMissStillNamesMentions: the permanent
// record posted from the DURABLE request must name people the same way the
// live card did.
//
// The sender decodes mentions once and caches the decoded text, so the live
// path is covered. authzd's durable record predates any Slack rendering and
// still holds "<@U…>", so the cache-miss path would otherwise post a record
// whose text silently downgrades to raw ids — the same message reading
// differently either side of a restart, which is the sort of difference nobody
// notices until they are comparing two screenshots during an incident.
func TestMetaagentColdStartResolution_CacheMissStillNamesMentions(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	require.NoError(t, metaagentaudit.RecordRequested(context.Background(), mem,
		memory.Scope{Kind: "session", ID: "ns/sess"}, metaagentaudit.Content{
			RequestID:   "req-888",
			Requester:   "U_BOB",
			RequestText: "<@U_BOT> widen scope so <@U_DANA> can see the board",
			ColdStart:   true,
			CleanedTask: "summarize the board for <@U_DANA>",
			ComposerOutput: metaagentaudit.ComposerOutput{
				ApproverSummary: "adds read access to the tracker",
			},
		}))

	api, posted := namingPostMessageAPI(t, map[string]string{
		"U_BOT":  "sre-bot",
		"U_DANA": "Dana Whitfield",
	})
	l := &slackListener{
		deps: channelkinds.Deps{
			Memory:      mem,
			NATSPublish: func(string, []byte) error { return nil },
		},
		api:                   api,
		idents:                NewIdentityCache(8),
		installedTeamID:       "T1",
		metaagentApprovalRefs: newMetaagentApprovalRefCache(), // empty: channelsd restarted
	}
	l.idents.Put(userInfo{UserID: "U_APPROVER", Email: "approver@example.com", TeamID: "T1"})

	cb := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		User:      slackapi.User{ID: "U_APPROVER"},
		Container: slackapi.Container{ChannelID: "CHAN", ThreadTs: "1700000000.000100"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{
			ActionID: "metaagent_approve_cleaned_req-888",
			Value:    MetaagentApprovalButtonValue("req-888", "ns/sess", MetaagentDecisionApproveCleaned),
		}}},
	}
	require.NoError(t,
		l.onInteraction(memory.WithSystemApproval(context.Background(), "test"), cb),
		"onInteraction")

	var permanent []postedMessage
	for _, p := range posted() {
		if strings.HasSuffix(p.path, "/chat.postMessage") {
			permanent = append(permanent, p)
		}
	}
	require.Len(t, permanent, 1, "the decision must leave a permanent in-thread record")
	assert.Contains(t, permanent[0].text, "@sre-bot", "the quoted request names the bot")
	assert.Contains(t, permanent[0].text, "@Dana Whitfield", "and the person it asked about")
	assert.NotContains(t, permanent[0].text, "U_BOT",
		"no raw id survives — decoded, not merely escaped")
	assert.NotContains(t, permanent[0].text, "<@",
		"and never as live markup: this text is the requester's, quoted")
}
