package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// showAgentUISuccess is the tool's success copy, pinned here because it is
// model-facing text a model repeats verbatim to a human: it may promise that
// the offer was SENT and nothing beyond that.
const showAgentUISuccess = "dashboard offer sent — the user can click to open it"

// showAgentUIPublish is one captured publish: the subject it went to and the
// exact bytes, so a test can decode the envelope rather than trust the
// producer's own struct.
type showAgentUIPublish struct {
	subject string
	payload []byte
}

// showAgentUIRecorder records every publish. Guarded because Execute's
// publisher is a plain func the tool may call from wherever it likes; the
// lock costs nothing and keeps the recorder honest under -race if that ever
// changes.
type showAgentUIRecorder struct {
	mu       sync.Mutex
	captured []showAgentUIPublish
}

func (r *showAgentUIRecorder) publish(_ context.Context, subject string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captured = append(r.captured, showAgentUIPublish{subject: subject, payload: append([]byte(nil), payload...)})
	return nil
}

func (r *showAgentUIRecorder) all() []showAgentUIPublish {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.captured
}

func showAgentUISession() *tool.SessionContext {
	return &tool.SessionContext{Namespace: "demo-ns", Name: "demo-session"}
}

// TestShowAgentUIExecute walks every verdict Execute can reach with a
// publisher wired, in the order Execute evaluates them. The rows that refuse
// all assert ZERO publishes as well as IsError: a refusal that still put an
// offer on the wire would leave the model apologising while the user's
// channel showed a button.
func TestShowAgentUIExecute(t *testing.T) {
	allow := func(context.Context) (bool, error) { return true, nil }
	deny := func(context.Context) (bool, error) { return false, nil }
	indeterminate := func(context.Context) (bool, error) { return false, errors.New("spicedb unavailable") }
	allowWithError := func(context.Context) (bool, error) { return true, errors.New("spicedb unavailable") }

	cases := []struct {
		name          string
		viewer        func(context.Context) (bool, error)
		session       *tool.SessionContext
		wantIsError   bool
		wantContains  string
		wantPublishes int
	}{
		{
			name:          "an authorized viewer gets an offer published",
			viewer:        allow,
			session:       showAgentUISession(),
			wantContains:  "sent",
			wantPublishes: 1,
		},
		{
			name:         "a denied viewer is refused and nothing is published",
			viewer:       deny,
			session:      showAgentUISession(),
			wantIsError:  true,
			wantContains: "not allowed to open this session's dashboard",
		},
		{
			// The page would refuse this viewer anyway, so the link is merely
			// useless rather than unsafe — but an agent that says "I've sent
			// you a link" to someone who then cannot open it is the worse
			// outcome, so an indeterminate answer refuses.
			name:         "an indeterminate check is a refusal, not an allow",
			viewer:       indeterminate,
			session:      showAgentUISession(),
			wantIsError:  true,
			wantContains: "could not confirm",
		},
		{
			// Not redundant with the row above: an Execute written as
			// `if !ok { refuse }` with the error inspected afterwards would
			// let exactly this pair through, and only a row supplying it can
			// see that.
			name:         "a check that errors while returning true is still a refusal",
			viewer:       allowWithError,
			session:      showAgentUISession(),
			wantIsError:  true,
			wantContains: "could not confirm",
		},
		{
			name:         "an unwired check is refused, not silently allowed",
			viewer:       nil,
			session:      showAgentUISession(),
			wantIsError:  true,
			wantContains: "not available",
		},
		{
			name:         "a session with no namespace is refused before publishing",
			viewer:       allow,
			session:      &tool.SessionContext{},
			wantIsError:  true,
			wantContains: "could not be identified",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &showAgentUIRecorder{}
			tl := meta.NewShowAgentUI(meta.ShowAgentUIConfig{
				NATSPublish:       rec.publish,
				ViewerCanInteract: tc.viewer,
			})

			res, err := tl.Execute(context.Background(), json.RawMessage(`{}`), tc.session)

			// A refusal is a Result, never a Go error: the model has to be
			// able to read and act on it.
			require.NoError(t, err, "Execute must not return a transport error on any of these paths")

			// assert, not require, from here down: the verdict, the copy and
			// the publish count are independent claims and a regression that
			// breaks more than one should report all of them.
			assert.Equal(t, tc.wantIsError, res.IsError, "IsError (content: %q)", res.Content)
			assert.True(t, res.Trusted, "every result this tool returns is framework-authored, never tool output")
			if tc.wantContains != "" {
				assert.Contains(t, res.Content, tc.wantContains, "result copy")
			}
			if !tc.wantIsError {
				// Pinned exactly, and the three words it must never gain are
				// named separately so a reword that APPENDS one fails on an
				// assertion that says why. A kind with no agent_ui_offer
				// sender drops the envelope with a log, so anything past
				// "sent" is a promise this tool cannot keep.
				assert.Equal(t, showAgentUISuccess, res.Content, "success copy")
				for _, forbidden := range []string{"delivered", "shown", "opened"} {
					assert.NotContains(t, res.Content, forbidden,
						"the offer is published, never %q — the tool cannot observe what a channel did with it", forbidden)
				}
			}
			assert.Len(t, rec.all(), tc.wantPublishes, "envelopes published")
			for _, p := range rec.all() {
				assert.True(t, strings.HasSuffix(p.subject, ".out.agent_ui_offer"),
					"published subject must be the agent_ui_offer leaf, got %q", p.subject)
			}
		})
	}
}

// TestShowAgentUIPublishesTheSessionsOwnRef is a claim about the PAYLOAD
// rather than about control flow, which is why it sits outside the table: the
// ref is "<ns>/<name>", assembled from the session context the runner handed
// the tool and never from anything the model can influence — the tool's schema
// accepts no arguments at all precisely so there is nothing to influence it
// with.
func TestShowAgentUIPublishesTheSessionsOwnRef(t *testing.T) {
	rec := &showAgentUIRecorder{}
	tl := meta.NewShowAgentUI(meta.ShowAgentUIConfig{
		NATSPublish:       rec.publish,
		ViewerCanInteract: func(context.Context) (bool, error) { return true, nil },
	})

	// A non-empty argument blob, to prove the tool ignores it: read_view does
	// the same, and a session id smuggled in here must change nothing.
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"session":"other-ns/other-session"}`), showAgentUISession())
	require.NoError(t, err, "Execute must succeed")
	require.False(t, res.IsError, "Execute must not refuse: %s", res.Content)
	require.Len(t, rec.all(), 1, "exactly one envelope")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(rec.all()[0].payload, &env), "published bytes must be an Envelope")
	assert.Equal(t, channelevents.KindAgentUIOffer, env.Kind, "envelope kind")

	var p channelevents.AgentUIOfferPayload
	require.NoError(t, json.Unmarshal(env.Payload, &p), "payload must be an AgentUIOfferPayload")
	assert.Equal(t, "demo-ns/demo-session", p.SessionRef,
		"the ref is the session the runner named, not one the model asked for")
}

// TestShowAgentUIPublishFailureLeaksNothing covers the one branch whose input
// is an ARBITRARY error rather than a value this package chose, which is the
// only place internal routing could reach model-facing text by way of data
// instead of code. A NATS client names the subject it failed on, and the model
// repeats what it is told, so the branch must return fixed copy and send the
// cause to the log.
//
// The failure is driven with an already-cancelled context because that is what
// makes the assertion below DISCRIMINATE rather than merely hold:
// publishWithRetry returns ctx.Err() from its first select, so the error
// reaching Execute is "context canceled" — a string that WOULD appear in the
// result if the branch ever went back to interpolating %v, and which a
// same-second test can produce without waiting out the 10s retry deadline.
func TestShowAgentUIPublishFailureLeaksNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	tl := meta.NewShowAgentUI(meta.ShowAgentUIConfig{
		NATSPublish: func(_ context.Context, subject string, _ []byte) error {
			attempts++
			// Shaped like the errors a NATS client actually returns, so the
			// assertions below are about a realistic leak rather than an
			// invented one.
			return errors.New(`nats: invalid subject "` + subject + `"`)
		},
		ViewerCanInteract: func(context.Context) (bool, error) { return true, nil },
	})

	res, err := tl.Execute(ctx, json.RawMessage(`{}`), showAgentUISession())

	require.NoError(t, err, "a publish failure is a refusal the model can read, never a Go error")
	require.Equal(t, 1, attempts, "the test must actually reach the publish branch")

	assert.True(t, res.IsError, "a failed publish must not report success")
	// The exact-copy pin is the discriminating half: reintroducing %v appends
	// the transport error and this fails immediately.
	assert.Equal(t, "show_agent_ui: the dashboard offer could not be sent. Do not tell the user a link is on its way; you can try again.",
		res.Content, "the refusal must be fixed copy, independent of the transport error")
	assert.NotContains(t, res.Content, "context canceled",
		"the transport error's own text must not reach the model")
	// These two name the rule the branch exists to keep. They are weaker than
	// the pin above — an empty string would satisfy them — but they state what
	// must never appear, so a future rewrite that reaches for the error text
	// again has to delete a stated rule rather than merely a string literal.
	assert.NotContains(t, res.Content, "ap.session", "no NATS subject in model-facing text")
	assert.NotContains(t, res.Content, "interact", "no SpiceDB relation in model-facing text")
}

// TestShowAgentUIWithoutAPublisherIsAWiringError asserts the one path that
// returns a Go error rather than a refusal. It is deliberately not a table
// row: an unset publisher is a wiring bug in the process, not an answer the
// model can act on, so it surfaces the way artifact_offer_view's does — as a
// real error the dispatcher wraps.
func TestShowAgentUIWithoutAPublisherIsAWiringError(t *testing.T) {
	tl := meta.NewShowAgentUI(meta.ShowAgentUIConfig{
		ViewerCanInteract: func(context.Context) (bool, error) { return true, nil },
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{}`), showAgentUISession())

	require.Error(t, err, "an unset publisher must not be swallowed as a refusal")
	assert.Contains(t, err.Error(), "NATSPublish", "the error must name the unset dependency")
	assert.Empty(t, res.Content, "no user- or model-facing copy is produced on a wiring failure")
}

// TestShowAgentUIShape pins the three declarations the runner and the model
// both read: the name (never one containing "modal" — a Slack modal is a
// views.open Block Kit surface that cannot host an HTTP-served page, so such a
// name would advertise an implementation that exists nowhere), the meta kind,
// and an InputSchema that accepts nothing.
func TestShowAgentUIShape(t *testing.T) {
	tl := meta.NewShowAgentUI(meta.ShowAgentUIConfig{})

	assert.Equal(t, "show_agent_ui", tl.Name(), "tool name")
	assert.NotContains(t, tl.Name(), "modal", "the name must not point at a Slack surface that cannot host this page")
	assert.Equal(t, tool.KindMeta, tl.Kind(), "tool kind")

	// The schema is asserted as BYTES, exactly, rather than decoded into a
	// struct and inspected field by field. Decoding cannot pin this: a schema
	// reduced to {"type":"object"} decodes to false/nil for the two keys that
	// matter, which is precisely what the field-by-field assertions were
	// checking for — so the keys could be deleted outright and nothing would
	// go red. JSONEq is exact in both directions, so it also catches a
	// property being added.
	assert.JSONEq(t, `{"type":"object","additionalProperties":false,"properties":{}}`, string(tl.InputSchema()),
		"the schema must take no arguments AND forbid extras — the model has no session id to supply")

	// Permission classification is what the toolguard and approval paths read
	// to decide how this call is handled; a silent move to Readonly or
	// Readwrite would reroute it with nothing else failing.
	assert.Equal(t, authz.Permission{StateImpact: authz.Passthrough}, tl.Permission(), "declared permission")
	assert.Nil(t, tl.PermissionVariants(), "this tool's classification does not depend on its arguments — it has none")

	// Description() is prompt, not documentation. Omitting "click" is what
	// produces an agent that tells the user it opened the page for them.
	desc := strings.ToLower(tl.Description())
	assert.Contains(t, desc, "click", "the description must say the user has to click")
	assert.Contains(t, desc, "cannot open it for them", "the description must say the agent cannot open it")
}
