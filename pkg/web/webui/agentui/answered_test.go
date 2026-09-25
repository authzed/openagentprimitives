package agentui

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// newAnsweredFixture builds a fresh in-memory backend for a fixed session
// scope, mirroring pkg/web/uiview/resolve_test.go's newMemFixture (and
// pkg/memory/kinds/uiviewmodel/accessor_test.go's identical fixture). The
// context carries a system approval because Memory.Put/Query gate on
// EnsureApproval unconditionally, independent of any pluggable Authorizer.
func newAnsweredFixture(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	return ctx, memory.NewLocal(inmem.NewBackend()), memory.Scope{Kind: "session", ID: "demo-ns/demo-session"}
}

// recordUserTurn writes one turn that resolves to a VISIBLE user message at
// the given moment — mirroring turn.RecordInbox's shape (role "inbox", a
// single text content block) but with a caller-chosen CreatedAt, since
// RecordInbox always stamps time.Now(). TestRecordUserTurnProducesAVisibleUserMessage,
// below, pins that this actually converts to a VisibleRoleUser entry before
// TestAnsweredHooks relies on it.
func recordUserTurn(t *testing.T, ctx context.Context, mem memory.Memory, scope memory.Scope, idx int, text string, at time.Time) error {
	t.Helper()
	return turn.NewAppender(mem, scope).Append(ctx, memory.Turn{
		Index: idx, Role: "inbox",
		Content:   []memory.ContentBlock{{Type: "text", Text: text}},
		CreatedAt: at,
	})
}

// TestRecordUserTurnProducesAVisibleUserMessage pins recordUserTurn's own
// contract before TestAnsweredHooks relies on it: the turn it writes must
// convert to a VisibleRoleUser entry at the given CreatedAt, not silently
// vanish or land under some other role.
func TestRecordUserTurnProducesAVisibleUserMessage(t *testing.T) {
	ctx, mem, scope := newAnsweredFixture(t)
	at := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	require.NoError(t, recordUserTurn(t, ctx, mem, scope, 0, "hello", at))

	turns, err := turn.ReadAll(ctx, mem, scope)
	require.NoError(t, err)
	msgs := turn.VisibleMessages(turns)
	require.Len(t, msgs, 1)
	assert.Equal(t, turn.VisibleRoleUser, msgs[0].Role)
	assert.True(t, at.Equal(msgs[0].CreatedAt))
}

func TestAnsweredHooks(t *testing.T) {
	t0 := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	mkView := func(composed map[string]time.Time) uicomponents.View {
		v := uicomponents.View{ComposedAt: composed}
		for h := range composed {
			v.AgentComposed = append(v.AgentComposed, h)
		}
		sort.Strings(v.AgentComposed)
		return v
	}
	cases := []struct {
		name     string
		composed map[string]time.Time
		userAt   []time.Time // visible user entries to record, in order
		want     []string
	}{
		{"no user message yet: nothing is answered", map[string]time.Time{"brief": t0}, nil, nil},
		{"the viewer spoke after the fill: answered", map[string]time.Time{"brief": t0}, []time.Time{t0.Add(time.Minute)}, []string{"brief"}},
		{"the viewer spoke before the fill: still open", map[string]time.Time{"brief": t0.Add(time.Minute)}, []time.Time{t0}, nil},
		{"only the older of two fills is answered", map[string]time.Time{"brief": t0, "tools": t0.Add(2 * time.Minute)}, []time.Time{t0.Add(time.Minute)}, []string{"brief"}},
		{"the latest user message decides, not the first", map[string]time.Time{"brief": t0.Add(time.Minute)}, []time.Time{t0, t0.Add(2 * time.Minute)}, []string{"brief"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, mem, scope := newAnsweredFixture(t)
			for i, at := range tc.userAt {
				require.NoError(t, recordUserTurn(t, ctx, mem, scope, i, "reply", at))
			}
			got, err := answeredHooks(ctx, mem, scope, mkView(tc.composed))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAnsweredHooksNeverListsAClearedHook proves the invariant answeredHooks'
// own doc comment states: a cleared hook has nothing to answer and is never
// listed, even though it IS composed and its clear predates the viewer's
// latest message — the two other conditions that would otherwise qualify it.
// Unlike TestAnsweredHooks above (which drives answeredHooks against a bare
// View with no Declaration), this one goes through a REAL uiview.Resolve so
// the cleared hook's node is really there, at zero children, for hookNode to
// find.
func TestAnsweredHooksNeverListsAClearedHook(t *testing.T) {
	ctx, mem, scope := newAnsweredFixture(t)
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{Slots: []spiceboxv1alpha1.AgentUISlot{
			{Name: "brief", AgentWritable: true},
		}},
	}
	clearedAt := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "brief", Cleared: true, WrittenAt: clearedAt,
	}))
	require.NoError(t, recordUserTurn(t, ctx, mem, scope, 0, "reply", clearedAt.Add(time.Minute)))

	v, err := uiview.Resolve(ctx, mem, scope, ui, uicomponents.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, []string{"brief"}, v.AgentComposed, "the clear itself must still be composed")

	got, err := answeredHooks(ctx, mem, scope, v)
	require.NoError(t, err)
	assert.Empty(t, got, "a cleared hook has nothing to answer")
}

// TestAnsweredHooksMarksAFilledHookThroughARealResolve is the filled-hook
// counterpart of the cleared-hook test above: a real uiview.Resolve, a real
// fill with content, a viewer message after it — answered.
func TestAnsweredHooksMarksAFilledHookThroughARealResolve(t *testing.T) {
	ctx, mem, scope := newAnsweredFixture(t)
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{Slots: []spiceboxv1alpha1.AgentUISlot{
			{Name: "brief", AgentWritable: true},
		}},
	}
	writtenAt := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "brief", Node: json.RawMessage(`{"component":"ap:markdown","props":{"body":"summary"}}`),
		WrittenAt: writtenAt,
	}))
	require.NoError(t, recordUserTurn(t, ctx, mem, scope, 0, "reply", writtenAt.Add(time.Minute)))

	v, err := uiview.Resolve(ctx, mem, scope, ui, uicomponents.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, []string{"brief"}, v.AgentComposed)

	got, err := answeredHooks(ctx, mem, scope, v)
	require.NoError(t, err)
	assert.Equal(t, []string{"brief"}, got)
}

// erroringMemory fails every Query — the shape of a transcript read that did
// not happen (a backend outage, a scope the facade refuses). Put/Search/
// SendSignal exist only so the type satisfies memory.Memory.
type erroringMemory struct{}

func (erroringMemory) Put(context.Context, memory.Entry) (memory.Entry, error) {
	return memory.Entry{}, errors.New("put refused")
}
func (erroringMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, errors.New("transcript unreadable")
}
func (erroringMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, errors.New("search refused")
}
func (erroringMemory) SendSignal(context.Context, memory.Signal) error { return nil }

var _ memory.Memory = erroringMemory{}

// The derivation is best-effort — a failed transcript read must not blank a
// page over a cosmetic cue — so the failure's only trace is this log line, and
// the line has to say WHICH page lost its answered cues. ns/name locate the
// session; `ui` is what separates two AgentUIs on the same session, and the
// sibling agentParamsFor log has carried it since it was written.
func TestAnsweredHooksForLogsTheUIItCouldNotDerive(t *testing.T) {
	var logged strings.Builder
	d := &liveFakeDeps{
		mem: erroringMemory{},
		logger: funcr.New(func(prefix, args string) {
			logged.WriteString(prefix)
			logged.WriteString(args)
			logged.WriteString("\n")
		}, funcr.Options{}),
	}

	got := answeredHooksFor(context.Background(), d, "demo-ns", "demo-session", "demo-ui", uicomponents.View{
		ComposedAt: map[string]time.Time{"brief": time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)},
	})

	assert.Nil(t, got, "a failed read marks nothing answered; every question simply shows")
	line := logged.String()
	assert.Contains(t, line, "could not compute answered hooks")
	assert.Contains(t, line, `"ns"="demo-ns"`)
	assert.Contains(t, line, `"name"="demo-session"`)
	assert.Contains(t, line, `"ui"="demo-ui"`)
	assert.Contains(t, line, "transcript unreadable", "the cause the operator greps for")
}
