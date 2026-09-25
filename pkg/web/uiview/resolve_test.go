package uiview_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// newMemFixture builds a fresh in-memory backend for a fixed session scope.
// The context carries a system approval because Memory.Put/Query gate on
// EnsureApproval unconditionally, independent of any pluggable Authorizer —
// see pkg/memory/kinds/uiaction/accessor_test.go's identical fixture.
func newMemFixture(t *testing.T) (context.Context, memory.Memory, memory.Scope) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	return ctx, memory.NewLocal(inmem.NewBackend()), memory.Scope{Kind: "session", ID: "demo-ns/demo-session"}
}

// writableUIFixture is an AgentUI with one agent-writable slot ("panel") and
// one that is not ("notes") — the minimum shape needed to exercise both the
// accept and reject paths through ResolveView. Through the spec.slots shim
// (uicomponents.CompileSlots) "panel" compiles to a hook of the same name;
// "notes" compiles to a plain, unnamed node — not a hook at all, so nothing
// can ever target it by name.
func writableUIFixture(t *testing.T) *spiceboxv1alpha1.AgentUI {
	t.Helper()
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "panel", AgentWritable: true},
				{Name: "notes", AgentWritable: false},
			},
		},
	}
}

// writableUIWithDefaultFixture is writableUIFixture's "panel" hook carrying a
// Tier-0 default, so a Clear has an author default to prove it does NOT fall
// back to.
func writableUIWithDefaultFixture(t *testing.T) *spiceboxv1alpha1.AgentUI {
	t.Helper()
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "panel", AgentWritable: true, Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:text","props":{"text":"author default"}}`)}},
				{Name: "notes", AgentWritable: false},
			},
		},
	}
}

// newFakeUIClient builds a controller-runtime fake client seeded with objs,
// scheme-registered for AgentUI — the same construction page_test.go's
// newFakeK8s uses.
func newFakeUIClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestResolveMergesStoredFragmentsAndReportsMalformedOnes(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	ui := writableUIFixture(t) // "panel" agentWritable, "notes" not

	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "panel", Node: json.RawMessage(`{"component":"ap:markdown","props":{"body":"agent"}}`),
		WrittenAt: time.Now().UTC()}))
	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "notes", Node: json.RawMessage(`{"component":"ap:text","props":{"text":"nope"}}`),
		WrittenAt: time.Now().UTC()}))

	v, err := uiview.Resolve(ctx, mem, scope, ui, uicomponents.DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, []string{"panel"}, v.AgentComposed)
	require.Len(t, v.Rejected, 1, "the non-writable slot's stored fragment must be reported, not dropped")
	assert.Equal(t, "notes", v.Rejected[0].Hook)
}

// TestResolveFillsComposedAtFromStoredWrittenAt pins uiview.Resolve's new
// obligation: every hook AgentComposed names gets a ComposedAt entry equal to
// its stored record's WrittenAt — a fill's timestamp for a written hook, a
// clear's for a cleared one — while a hook the agent never touched (an author
// default) is simply absent from the map, not zero-valued.
func TestResolveFillsComposedAtFromStoredWrittenAt(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{Slots: []spiceboxv1alpha1.AgentUISlot{
			{Name: "panel", AgentWritable: true},
			{Name: "aside", AgentWritable: true},
			{Name: "notes", AgentWritable: false},
		}},
	}
	writtenAt := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	clearedAt := time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC)

	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "panel", Node: json.RawMessage(`{"component":"ap:markdown","props":{"body":"agent"}}`),
		WrittenAt: writtenAt,
	}))
	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "aside", Cleared: true, WrittenAt: clearedAt,
	}))

	v, err := uiview.Resolve(ctx, mem, scope, ui, uicomponents.DefaultOptions())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"aside", "panel"}, v.AgentComposed)

	require.Contains(t, v.ComposedAt, "panel")
	assert.True(t, writtenAt.Equal(v.ComposedAt["panel"]))
	require.Contains(t, v.ComposedAt, "aside")
	assert.True(t, clearedAt.Equal(v.ComposedAt["aside"]))
	assert.NotContains(t, v.ComposedAt, "notes", "a hook the agent never composed must be absent, not zero-valued")
}

func TestResolveFailsClosedOnNilMemory(t *testing.T) {
	var mem memory.Memory // a genuine nil interface, not a typed-nil pointer
	_, err := uiview.Resolve(context.Background(), mem, memory.Scope{Kind: "session", ID: "demo-ns/demo-session"},
		writableUIFixture(t), uicomponents.DefaultOptions())
	require.Error(t, err, `"no memory backend" and "the agent composed nothing" must not render the same`)
}

// TestResolveReportsAStoredFragmentThatNoLongerParses pins the OTHER half of
// "never skipped": a record whose Node is no longer valid JSON for ParseNode
// (the vocabulary shed a field between write and read) must still surface as
// a Rejection, not vanish silently while the slot quietly falls back to
// Tier 0.
func TestResolveReportsAStoredFragmentThatNoLongerParses(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	ui := writableUIFixture(t)

	require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
		UI: "demo-ui", Slot: "panel", Node: json.RawMessage(`{"component":"ap:text","unknownField":1}`),
		WrittenAt: time.Now().UTC()}))

	v, err := uiview.Resolve(ctx, mem, scope, ui, uicomponents.DefaultOptions())
	require.NoError(t, err)
	assert.Empty(t, v.AgentComposed)
	require.Len(t, v.Rejected, 1)
	assert.Equal(t, "panel", v.Rejected[0].Hook)
}

func TestRuntimeOptionsFailsClosedWhenUnattached(t *testing.T) {
	rt := &uiview.Runtime{} // ToolOptions nil: constructed, never attached
	o := rt.Options()
	assert.Empty(t, o.GrantedTools, "an unattached Runtime must grant zero tools, never all of them")
	assert.Empty(t, o.ReadonlyTools)
}

func TestRuntimeWriteAcceptsAHookAndReplacesOnSecondWrite(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	ui := writableUIFixture(t)
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeUIClient(t, ui),
		Mem:    mem,
	}

	v, err := rt.Write(ctx, "panel", uicomponents.Node{
		Component: "ap:markdown",
		Props:     map[string]json.RawMessage{"body": json.RawMessage(`"first"`)},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"panel"}, v.AgentComposed)

	v2, err := rt.Write(ctx, "panel", uicomponents.Node{
		Component: "ap:markdown",
		Props:     map[string]json.RawMessage{"body": json.RawMessage(`"second"`)},
	})
	require.NoError(t, err)
	// If Write appended the proposal instead of replacing the stored
	// fragment for "panel", ResolveView would see two candidates for the
	// same slot and AgentComposed would carry "panel" twice — a symptom
	// uiviewmodel.List can't see, since Record's own deterministic entry ID
	// always collapses storage to one record regardless of what Write fed
	// ResolveView.
	assert.Equal(t, []string{"panel"}, v2.AgentComposed, "a second Write to the same slot must REPLACE, never accumulate")

	stored, err := uiviewmodel.List(ctx, mem, scope, "demo-ui")
	require.NoError(t, err)
	require.Len(t, stored, 1, "a second Write to the same slot must REPLACE, never accumulate")
	assert.JSONEq(t, `{"component":"ap:markdown","props":{"body":"second"}}`, string(stored[0].Node))
}

// captureSlog swaps the process default slog handler for one writing into a
// buffer, restoring it on cleanup, and returns a reader for what was logged.
// Global state, so no test using it may run parallel with another — none in
// this package does.
func captureSlog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestRuntimeWriteReportsAnUnrelatedStoredFragmentThatNoLongerParses is the
// WRITE-path half of loadFragments' contract. Resolve already reported these;
// Write discarded loadFragments' second return entirely, so a slot that
// silently reverted to Tier 0 (the vocabulary shed a wire field between the
// write and this read) was invisible to the agent AND to the operator until
// somebody happened to read the view.
//
// Two arms, because "report every parse rejection" and "report the one this
// write is fixing" are different claims and only the first is true.
func TestRuntimeWriteReportsAnUnrelatedStoredFragmentThatNoLongerParses(t *testing.T) {
	// Both slots agentWritable, so a rejection for the untouched slot can only
	// be a PARSE rejection — an unparseable record never reaches ResolveView
	// to be rejected on any other ground.
	twoWritable := func(t *testing.T) *spiceboxv1alpha1.AgentUI {
		t.Helper()
		return &spiceboxv1alpha1.AgentUI{
			ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
			Spec: spiceboxv1alpha1.AgentUISpec{Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "panel", AgentWritable: true},
				{Name: "aside", AgentWritable: true},
			}},
		}
	}

	t.Run("a write to another slot reports it to the agent and logs it", func(t *testing.T) {
		ctx, mem, scope := newMemFixture(t)
		logged := captureSlog(t)
		rt := &uiview.Runtime{
			Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
			Client: newFakeUIClient(t, twoWritable(t)), Mem: mem,
		}
		require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
			UI: "demo-ui", Slot: "aside", Node: json.RawMessage(`{"component":"ap:text","unknownField":1}`),
			WrittenAt: time.Now().UTC()}))

		v, err := rt.Write(ctx, "panel", uicomponents.Node{
			Component: "ap:markdown",
			Props:     map[string]json.RawMessage{"body": json.RawMessage(`"fresh"`)},
		})
		require.NoError(t, err, "an unrelated slot's unparseable record must not fail this write")
		assert.Equal(t, []string{"panel"}, v.AgentComposed)

		require.Len(t, v.Rejected, 1, "the agent is told which of its own slots is no longer being served")
		assert.Equal(t, "aside", v.Rejected[0].Hook)
		assert.NotEmpty(t, v.Rejected[0].Reason)

		out := logged()
		assert.Contains(t, out, "no longer parses", "an operator must be able to grep for this")
		assert.Contains(t, out, "hook=aside")
		assert.Contains(t, out, "session=demo-session")
	})

	t.Run("a write to the SAME slot retires its own rejection", func(t *testing.T) {
		ctx, mem, scope := newMemFixture(t)
		rt := &uiview.Runtime{
			Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
			Client: newFakeUIClient(t, twoWritable(t)), Mem: mem,
		}
		require.NoError(t, uiviewmodel.Record(ctx, mem, scope, uiviewmodel.Content{
			UI: "demo-ui", Slot: "aside", Node: json.RawMessage(`{"component":"ap:text","unknownField":1}`),
			WrittenAt: time.Now().UTC()}))

		v, err := rt.Write(ctx, "aside", uicomponents.Node{
			Component: "ap:markdown",
			Props:     map[string]json.RawMessage{"body": json.RawMessage(`"repaired"`)},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"aside"}, v.AgentComposed)
		assert.Empty(t, v.Rejected,
			"this write overwrote the record that would not parse; reporting it would name a slot at the moment it stops being broken")
	})
}

// TestRuntimeWriteRejectsAnUnknownHookAndWritesNothing replaces the old
// "non-writable slot" case: through the shim, a non-agent-writable slot
// compiles to a plain node with no name at all, so there is no "not writable"
// outcome to distinguish — targeting it is indistinguishable from targeting a
// hook that was never declared.
func TestRuntimeWriteRejectsAnUnknownHookAndWritesNothing(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	ui := writableUIFixture(t) // slots: "panel" writable, "notes" not
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: ui.Name,
		Client: newFakeUIClient(t, ui), Mem: mem,
	}

	_, err := rt.Write(ctx, "notes", uicomponents.Node{Component: "ap:text"})
	var verr *uicomponents.ValidationError
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, "hooks[notes]", verr.Path)
	assert.Contains(t, verr.Reason, `unknown hook "notes"`, "a non-writable legacy slot is not a hook at all")

	stored, lerr := uiviewmodel.List(ctx, mem, scope, ui.Name)
	require.NoError(t, lerr)
	assert.Empty(t, stored, "nothing written")
}

// TestRuntimeClearRecordsAnIntentionalEmptyAndResolvesToIt pins Clear as a
// real write: it is durable (List sees exactly one record, Cleared), it
// composes (AgentComposed names the hook), and a fresh Resolve sees the
// clear rather than falling back to the author's Tier-0 default.
func TestRuntimeClearRecordsAnIntentionalEmptyAndResolvesToIt(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	ui := writableUIWithDefaultFixture(t) // "panel" writable with an ap:text default
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: ui.Name,
		Client: newFakeUIClient(t, ui), Mem: mem,
	}

	v, err := rt.Clear(ctx, "panel")
	require.NoError(t, err)
	assert.Equal(t, []string{"panel"}, v.AgentComposed)

	stored, err := uiviewmodel.List(ctx, mem, scope, ui.Name)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.True(t, stored[0].Cleared)
	assert.Empty(t, stored[0].Node)

	// A fresh Resolve sees the clear, not the author default.
	got, err := uiview.Resolve(ctx, mem, scope, ui, uicomponents.DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, []string{"panel"}, got.AgentComposed)
	h := uicomponents.Hooks(got.Declaration)[0]
	n := *got.Declaration.View
	for _, i := range h.Path {
		n = n.Children[i]
	}
	assert.Empty(t, n.Children)
}

// TestRuntimeWriteAfterClearReplacesTheClear pins that a hook cleared and
// then written again lands as ONE record, not two — the same replace-in-place
// contract an ordinary second Write has (TestRuntimeWriteAcceptsAHookAndReplacesOnSecondWrite),
// now proven across a Clear/Write pair.
func TestRuntimeWriteAfterClearReplacesTheClear(t *testing.T) {
	ctx, mem, scope := newMemFixture(t)
	ui := writableUIWithDefaultFixture(t)
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: ui.Name,
		Client: newFakeUIClient(t, ui), Mem: mem,
	}

	_, err := rt.Clear(ctx, "panel")
	require.NoError(t, err)

	_, err = rt.Write(ctx, "panel", uicomponents.Node{
		Component: "ap:text",
		Props:     map[string]json.RawMessage{"text": json.RawMessage(`"back"`)},
	})
	require.NoError(t, err)

	stored, err := uiviewmodel.List(ctx, mem, scope, ui.Name)
	require.NoError(t, err)
	require.Len(t, stored, 1, "one record per hook, replaced in place")
	assert.False(t, stored[0].Cleared)
}

// TestRuntimeWriteFailsWhenAnUnrelatedSlotDefaultCarriesAnUnknownWireField
// pins that Write's base conversion is the SAME strict ParseDeclaration path
// Resolve uses — not a second, hand-rolled field copy. A field-copying
// conversion would silently drop an unrecognized key on a slot the write
// never even touches, and the merged document would validate clean on data
// that was never actually legal.
func TestRuntimeWriteFailsWhenAnUnrelatedSlotDefaultCarriesAnUnknownWireField(t *testing.T) {
	ctx, mem, _ := newMemFixture(t)
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "panel", AgentWritable: true},
				{Name: "broken", Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:text","childs":[]}`)}},
			},
		},
	}
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeUIClient(t, ui),
		Mem:    mem,
	}

	_, err := rt.Write(ctx, "panel", uicomponents.Node{
		Component: "ap:text",
		Props:     map[string]json.RawMessage{"text": json.RawMessage(`"hi"`)},
	})
	assert.Error(t, err, "a field-copying second conversion path would accept and silently drop the unrecognized field on an unrelated slot")
}
