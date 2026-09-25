package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// csPlacement records what the hook's SetPlacement callback received.
type csPlacement struct {
	called  bool
	place   bool
	content []memory.ContentBlock
}

// startInput builds the SessionStart Input carrying the initial prompt.
func startInput(text string) pipeline.Input {
	return pipeline.Input{
		Point:   pipeline.SessionStart,
		Session: pipeline.SessionRef{Namespace: "default", Name: "rcs1"},
		Turn:    &pipeline.TurnInfo{Text: text},
	}
}

// newColdStartScope builds a ColdStartScope hook over a shared in-mem memory,
// a recording publish func, and a placement sink. The autoApply flag selects
// extractAndApprove (false) vs extractAndAutoApply (true). The wait deadlines
// are pinned short so fail-closed/timeout cases resolve fast.
func newColdStartScope(t *testing.T, autoApply bool, capturedPayload *[]byte, sink *csPlacement, publishErr error) (*hooks.ColdStartScope, memory.Memory, memory.Scope) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	scopeRef := memory.Scope{Kind: "session", ID: "default/rcs1"}

	d := hooks.ColdStartScopeDeps{
		Requester:       identity.CanonicalFromTrusted("user:alice", "test fixture"),
		AutoApply:       autoApply,
		ApprovalTimeout: 200 * time.Millisecond,
		LLMLatency:      200 * time.Millisecond,
		BoundEntities: []scope.EnvelopeBoundEntity{
			{ResourceType: "github_repo", Permission: "read"},
		},
		ToolNames: []string{"github_list_prs", "send_email"},
		Publish: func(_ context.Context, payload []byte) error {
			if capturedPayload != nil {
				*capturedPayload = append([]byte(nil), payload...)
			}
			return publishErr
		},
		WaitForTask: func(ctx context.Context, deadline time.Duration) {
			_ = authz.WaitForColdStartTask(ctx, mem, scopeRef, deadline)
		},
		GetTask: func(ctx context.Context) (coldstarttask.Content, bool, error) {
			return coldstarttask.Get(ctx, mem, scopeRef)
		},
		SetPlacement: func(place bool, content []memory.ContentBlock) {
			sink.called = true
			sink.place = place
			sink.content = content
		},
	}
	return hooks.NewColdStartScope(d), mem, scopeRef
}

// TestColdStartScope_Eval is the hook-level port of TestRunColdStart: it drives
// Eval against a pre-seeded cold_start_task per decision and asserts the
// placement callback + verdict + published payload.
func TestColdStartScope_Eval(t *testing.T) {
	const rawText = "open 5 PRs and email the board"

	cases := []struct {
		name      string
		cst       *coldstarttask.Content // nil → no cold_start_task (timeout / fail-closed)
		wantHalt  bool                   // true → fail-closed: Halt + Notice, NO placement
		wantPlace bool
		wantText  string
	}{
		{
			name:      "approved_cleaned: place cleaned text (Allow)",
			cst:       &coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: "open 1 PR"},
			wantPlace: true,
			wantText:  "open 1 PR",
		},
		{
			name:      "approved_cleaned empty: place=false (Allow)",
			cst:       &coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: ""},
			wantPlace: false,
		},
		{
			name:      "approved_original: place raw (Allow)",
			cst:       &coldstarttask.Content{Status: coldstarttask.StatusApprovedOriginal},
			wantPlace: true,
			wantText:  rawText,
		},
		{
			name:      "ran_without_scope: place raw (Allow)",
			cst:       &coldstarttask.Content{Status: coldstarttask.StatusRanWithoutScope},
			wantPlace: true,
			wantText:  rawText,
		},
		{
			name:      "denied: place=false, Allow (NOT Halt — authzd surfaced deny)",
			cst:       &coldstarttask.Content{Status: coldstarttask.StatusDenied},
			wantPlace: false,
		},
		{
			name:     "no cold_start_task (timeout): Halt + Notice, no placement",
			cst:      nil,
			wantHalt: true,
		},
		{
			name:     "scope_review_failed: Halt + Notice, no placement",
			cst:      &coldstarttask.Content{Status: coldstarttask.StatusScopeReviewFailed},
			wantHalt: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			var payload []byte
			sink := &csPlacement{}
			h, mem, scopeRef := newColdStartScope(t, false, &payload, sink, nil)
			if tc.cst != nil {
				require.NoError(t, coldstarttask.Put(ctx, mem, scopeRef, *tc.cst))
			}

			dec := h.Eval(ctx, startInput(rawText))

			if tc.wantHalt {
				assert.Equal(t, pipeline.Halt, dec.Verdict, "fail-closed → Halt")
				require.NotEmpty(t, dec.Notices, "fail-closed must carry a user-facing Notice")
				assert.NotEmpty(t, dec.Notices[0].Text, "notice text must be non-empty")
				assert.False(t, sink.called, "fail-closed must NOT set placement")
			} else {
				assert.Equal(t, pipeline.Allow, dec.Verdict, "usable decision → Allow")
				require.True(t, sink.called, "usable decision must set placement")
				assert.Equal(t, tc.wantPlace, sink.place, "placement decision")
				if tc.wantPlace {
					require.NotEmpty(t, sink.content, "placed turn must carry content")
					assert.Equal(t, tc.wantText, sink.content[0].Text, "placed content text")
				}
			}

			// The metaagent_request is always published, regardless of decision.
			require.NotEmpty(t, payload, "metaagent_request must be published")
			var got struct {
				Requester string          `json:"requester"`
				Text      string          `json:"text"`
				ColdStart bool            `json:"coldStart"`
				Envelope  json.RawMessage `json:"envelope"`
			}
			require.NoError(t, json.Unmarshal(payload, &got))
			assert.True(t, got.ColdStart, "payload carries coldStart:true")
			assert.Equal(t, rawText, got.Text)
			assert.Equal(t, "user:alice", got.Requester)
			assert.NotEmpty(t, got.Envelope, "payload carries a non-empty envelope")
			assert.Contains(t, string(got.Envelope), "github_list_prs", "envelope carries tool names")
			assert.Contains(t, string(got.Envelope), "github_repo", "envelope carries bound entity types")
		})
	}
}

// TestColdStartScope_AutoApplyStaysOffTheWire pins the trust boundary: whether
// the human approval gate is waived is authzd's call, resolved from the
// session's authz_session_config snapshot. The runner must NOT put it on
// ap.session.<ns>.<name>.in.metaagent_request — a subject inside its own NATS
// grant, so a flag sent from here would be the component the gate constrains
// asking to skip it.
//
// The previous version of this test asserted the opposite ("extractAndAutoApply
// → autoApply true" on the payload); it pinned the defect as intent.
// AutoApply's remaining job is local and unchanged — it sizes the runner's own
// wait deadline — and is asserted by TestColdStartScope_AutoApplySizesTheWait.
func TestColdStartScope_AutoApplyStaysOffTheWire(t *testing.T) {
	for _, autoApply := range []bool{false, true} {
		name := "extractAndApprove: payload carries no autoApply key"
		if autoApply {
			name = "extractAndAutoApply: payload STILL carries no autoApply key"
		}
		t.Run(name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			var payload []byte
			sink := &csPlacement{}
			h, mem, scopeRef := newColdStartScope(t, autoApply, &payload, sink, nil)
			require.NoError(t, coldstarttask.Put(ctx, mem, scopeRef,
				coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: "x"}))

			_ = h.Eval(ctx, startInput("do a thing"))
			require.NotEmpty(t, payload)
			var got map[string]any
			require.NoError(t, json.Unmarshal(payload, &got))
			assert.NotContains(t, got, "autoApply",
				"the runner must not tell authzd to skip the human approval gate")
		})
	}
}

// TestColdStartScope_AutoApplySizesTheWait pins Deps.AutoApply's one remaining
// job now that it is off the wire: it selects how long the runner blocks for
// authzd's decision. extractAndAutoApply has no human in the loop, so it waits
// the machine-speed extractor latency; extractAndApprove waits the human
// approval window. Getting this backwards makes a class either halt legitimate
// sessions early or stall every one of them for the full approval window.
func TestColdStartScope_AutoApplySizesTheWait(t *testing.T) {
	const approvalWindow = 7 * time.Minute
	const llmLatency = 3 * time.Second

	cases := []struct {
		name      string
		autoApply bool
		want      time.Duration
	}{
		{name: "extractAndApprove: waits the human approval window", autoApply: false, want: approvalWindow},
		{name: "extractAndAutoApply: waits the extractor latency", autoApply: true, want: llmLatency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			mem := memory.NewLocal(inmem.NewBackend())
			scopeRef := memory.Scope{Kind: "session", ID: "default/wait-test"}
			// A decision is already present, so WaitForTask returns at once and
			// the test observes the requested deadline without spending it.
			require.NoError(t, coldstarttask.Put(ctx, mem, scopeRef,
				coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: "x"}))

			var gotDeadline time.Duration
			h := hooks.NewColdStartScope(hooks.ColdStartScopeDeps{
				Requester:       identity.CanonicalFromTrusted("user:alice", "test fixture"),
				AutoApply:       tc.autoApply,
				ApprovalTimeout: approvalWindow,
				LLMLatency:      llmLatency,
				Publish:         func(context.Context, []byte) error { return nil },
				WaitForTask: func(_ context.Context, deadline time.Duration) {
					gotDeadline = deadline
				},
				GetTask: func(ctx context.Context) (coldstarttask.Content, bool, error) {
					return coldstarttask.Get(ctx, mem, scopeRef)
				},
				SetPlacement: func(bool, []memory.ContentBlock) {},
			})

			h.Eval(ctx, startInput("do a thing"))
			assert.Equal(t, tc.want, gotDeadline, "wait deadline")
		})
	}
}

// TestColdStartScope_PublishFailsClosed asserts a publish error fails CLOSED:
// Halt + a user-facing Notice, no placement set.
func TestColdStartScope_PublishFailsClosed(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sink := &csPlacement{}
	h, _, _ := newColdStartScope(t, false, nil, sink, errors.New("publish boom"))

	dec := h.Eval(ctx, startInput("raw text"))
	assert.Equal(t, pipeline.Halt, dec.Verdict, "publish failure must Halt")
	require.NotEmpty(t, dec.Notices, "publish failure must notify the user")
	assert.False(t, sink.called, "publish failure must not set placement")
}

// TestColdStartScope_NameAndPoints pins the hook identity.
func TestColdStartScope_NameAndPoints(t *testing.T) {
	h := hooks.NewColdStartScope(hooks.ColdStartScopeDeps{})
	assert.Equal(t, "cold_start_scope", h.Name())
	assert.Equal(t, []pipeline.Point{pipeline.SessionStart}, h.Points())
}

// TestColdStartScope_LifecycleCallbacks verifies that OnScopeReviewAsked is
// called after a successful publish and OnScopeReviewResolved is called with
// the correct (approved, timedOut) pair for each outcome variant.
func TestColdStartScope_LifecycleCallbacks(t *testing.T) {
	cases := []struct {
		name         string
		cst          *coldstarttask.Content // nil → no task (timeout)
		publishErr   error
		wantAsked    bool
		wantResolved bool
		wantApproved bool
		wantTimedOut bool
	}{
		{
			name:         "approved: asked=true, resolved approved=true timedOut=false",
			cst:          &coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: "ok"},
			wantAsked:    true,
			wantResolved: true,
			wantApproved: true,
			wantTimedOut: false,
		},
		{
			name:         "denied: asked=true, resolved approved=false timedOut=false",
			cst:          &coldstarttask.Content{Status: coldstarttask.StatusDenied},
			wantAsked:    true,
			wantResolved: true,
			wantApproved: false,
			wantTimedOut: false,
		},
		{
			name:         "scope_review_failed: asked=true, resolved approved=false timedOut=false",
			cst:          &coldstarttask.Content{Status: coldstarttask.StatusScopeReviewFailed},
			wantAsked:    true,
			wantResolved: true,
			wantApproved: false,
			wantTimedOut: false,
		},
		{
			name:         "timeout (no task): asked=true, resolved approved=false timedOut=true",
			cst:          nil, // deadline elapses before authzd writes the task
			wantAsked:    true,
			wantResolved: true,
			wantApproved: false,
			wantTimedOut: true,
		},
		{
			name:         "publish error: asked=false, resolved=false (no in-flight record)",
			publishErr:   errors.New("publish down"),
			wantAsked:    false,
			wantResolved: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			mem := memory.NewLocal(inmem.NewBackend())
			scopeRef := memory.Scope{Kind: "session", ID: "default/cb-test"}

			var asked bool
			var resolved bool
			var gotApproved, gotTimedOut bool

			d := hooks.ColdStartScopeDeps{
				Requester:       identity.CanonicalFromTrusted("user:alice", "test fixture"),
				ApprovalTimeout: 100 * time.Millisecond,
				LLMLatency:      100 * time.Millisecond,
				Publish:         func(_ context.Context, _ []byte) error { return tc.publishErr },
				WaitForTask: func(ctx context.Context, deadline time.Duration) {
					_ = authz.WaitForColdStartTask(ctx, mem, scopeRef, deadline)
				},
				GetTask: func(ctx context.Context) (coldstarttask.Content, bool, error) {
					return coldstarttask.Get(ctx, mem, scopeRef)
				},
				SetPlacement: func(bool, []memory.ContentBlock) {},
				OnScopeReviewAsked: func(_ context.Context) {
					asked = true
				},
				OnScopeReviewResolved: func(_ context.Context, approved, timedOut bool) {
					resolved = true
					gotApproved = approved
					gotTimedOut = timedOut
				},
			}
			if tc.cst != nil {
				require.NoError(t, coldstarttask.Put(ctx, mem, scopeRef, *tc.cst))
			}

			h := hooks.NewColdStartScope(d)
			h.Eval(ctx, startInput("test request"))

			assert.Equal(t, tc.wantAsked, asked, "OnScopeReviewAsked call")
			assert.Equal(t, tc.wantResolved, resolved, "OnScopeReviewResolved call")
			if tc.wantResolved {
				assert.Equal(t, tc.wantApproved, gotApproved, "resolved: approved")
				assert.Equal(t, tc.wantTimedOut, gotTimedOut, "resolved: timedOut")
			}
		})
	}
}

// TestColdStartScope_TurnContent is the pure status→placement table, moved from
// the runner (the helper now lives in the hook package).
func TestColdStartScope_TurnContent(t *testing.T) {
	raw := []memory.ContentBlock{{Type: "text", Text: "open 5 PRs and email the board"}}

	cases := []struct {
		name      string
		cst       coldstarttask.Content
		wantPlace bool
		wantText  string
	}{
		{
			name:      "approved_cleaned: places the cleaned task text",
			cst:       coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: "open 1 PR"},
			wantPlace: true,
			wantText:  "open 1 PR",
		},
		{
			name:      "approved_cleaned with empty cleaned text: places nothing",
			cst:       coldstarttask.Content{Status: coldstarttask.StatusApprovedCleaned, CleanedText: ""},
			wantPlace: false,
		},
		{
			name:      "approved_original: places the raw content",
			cst:       coldstarttask.Content{Status: coldstarttask.StatusApprovedOriginal},
			wantPlace: true,
			wantText:  "open 5 PRs and email the board",
		},
		{
			name:      "ran_without_scope: places the raw content",
			cst:       coldstarttask.Content{Status: coldstarttask.StatusRanWithoutScope},
			wantPlace: true,
			wantText:  "open 5 PRs and email the board",
		},
		{
			name:      "denied: places nothing",
			cst:       coldstarttask.Content{Status: coldstarttask.StatusDenied},
			wantPlace: false,
		},
		{
			name:      "unknown status: places nothing (conservative)",
			cst:       coldstarttask.Content{Status: "something_unexpected"},
			wantPlace: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			place, content := hooks.ColdStartTurnContent(tc.cst, raw)
			assert.Equal(t, tc.wantPlace, place, "placement decision")
			if tc.wantPlace {
				require.NotEmpty(t, content, "placed turn must carry content")
				assert.Equal(t, tc.wantText, content[0].Text, "placed content text")
			}
		})
	}
}

// TestColdStartScope_Envelope asserts the envelope is built from the supplied
// bound entities + tool names (pre-resolved by the runner; the hook never reads
// the AgentClass), and is empty when both are empty.
func TestColdStartScope_Envelope(t *testing.T) {
	t.Run("empty inputs: empty envelope", func(t *testing.T) {
		env := hooks.ColdStartEnvelope(nil, nil)
		assert.Empty(t, env.Tools)
		assert.Empty(t, env.BoundEntities)
	})

	t.Run("populated from bound entities + tool names", func(t *testing.T) {
		env := hooks.ColdStartEnvelope(
			[]scope.EnvelopeBoundEntity{
				{ResourceType: "github_repo", Permission: "read"},
				{ResourceType: "linear_team", Permission: "view"},
			},
			[]string{"a", "b"},
		)
		require.Len(t, env.Tools, 2)
		assert.Equal(t, "a", env.Tools[0].Name)
		assert.Equal(t, "b", env.Tools[1].Name)
		require.Len(t, env.BoundEntities, 2)
		assert.Equal(t, "github_repo", env.BoundEntities[0].ResourceType)
		assert.Equal(t, "read", env.BoundEntities[0].Permission)
		assert.Equal(t, "linear_team", env.BoundEntities[1].ResourceType)
	})
}
