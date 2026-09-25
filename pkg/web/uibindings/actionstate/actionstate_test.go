package actionstate_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/actionstate"
)

const (
	fixtureNamespace = "demo-ns"
	fixtureSession   = "demo-session"
)

var (
	older = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newer = time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
)

// fakeMemory hands back whatever entries depsWith seeded it with, ignoring
// the incoming memory.Query entirely (no scope/kind filtering, no capability
// door) — uiaction.List does its own Requester filtering in Go after the
// Query returns, which is the behavior under test here, not the backend's.
// Modeled on memoryref_test.go's fakeMemory; unlike the real *memory.Local
// facade (pkg/memory/facade.go), this fake has no ensureApproval capability
// door, so Resolve's own context (t.Context(), no minted approval) reaches
// it exactly as a production call would reach the real facade's HTTP-fronted
// twin (webd's Deps.Memory() is an httpclient, not the in-process facade —
// authorization there is the scoped bearer token, not a Go-context grant).
type fakeMemory struct {
	entries []memory.Entry
}

func (f *fakeMemory) Put(context.Context, memory.Entry) (memory.Entry, error) {
	return memory.Entry{}, nil
}
func (f *fakeMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{Entries: f.entries}, nil
}
func (f *fakeMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (f *fakeMemory) SendSignal(context.Context, memory.Signal) error { return nil }

// fakeDeps' mem field is typed as the memory.Memory INTERFACE (not a
// pointer), so the zero value fakeDeps{} carries a genuine nil interface —
// exactly the case the resolver's fail-closed guard must catch without a
// typed-nil false negative. Mirrors memoryref_test.go's fakeDeps exactly.
type fakeDeps struct {
	mem memory.Memory
}

func (f fakeDeps) Memory() memory.Memory                                   { return f.mem }
func (f fakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (f fakeDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (f fakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (f fakeDeps) Logger() logr.Logger                                     { return logr.Discard() }

// depsWith builds a uibindings.Deps whose Memory() holds exactly contents,
// marshaled as ui_action entries. nil/empty contents is the "nobody has
// clicked anything yet" fixture.
func depsWith(t *testing.T, contents []uiaction.Content) uibindings.Deps {
	t.Helper()
	entries := make([]memory.Entry, 0, len(contents))
	for _, c := range contents {
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		entries = append(entries, memory.Entry{
			Scope:     memory.Scope{Kind: "session", ID: fixtureNamespace + "/" + fixtureSession},
			Kind:      uiaction.KindName,
			ID:        uiaction.EntryID(c.RequestID),
			CreatedAt: c.UpdatedAt,
			Content:   raw,
		})
	}
	return fakeDeps{mem: &fakeMemory{entries: entries}}
}

// depsWithNilMemory returns a Deps whose Memory() is a genuine nil
// interface — the zero value of fakeDeps, never a typed-nil pointer boxed
// into the interface.
func depsWithNilMemory(t *testing.T) uibindings.Deps {
	t.Helper()
	return fakeDeps{}
}

func reqFor(t *testing.T, subject, ref string) uibindings.Request {
	t.Helper()
	return uibindings.Request{
		Namespace: fixtureNamespace,
		Session:   fixtureSession,
		Subject:   subject,
		Path:      "root/#body",
		Ref:       ref,
	}
}

// quoted renders s as the JSON string literal Resolve's result carries, so a
// test can assert.JSONEq it against res.Value directly.
func quoted(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// resolveCopy resolves a single-record fixture and returns the decoded
// human-copy string (not the raw JSON bytes), for tests that inspect the
// text itself rather than compare it against another DisplayCopy call.
func resolveCopy(t *testing.T, subject string, c uiaction.Content) string {
	t.Helper()
	res, err := actionstate.New().Resolve(t.Context(), depsWith(t, []uiaction.Content{c}), reqFor(t, subject, c.Action))
	require.NoError(t, err)
	var got string
	require.NoError(t, json.Unmarshal(res.Value, &got))
	return got
}

func TestActionStateResolve(t *testing.T) {
	const viewer = "user:viewer-1"

	assert.Equal(t, "action", actionstate.New().Source())

	t.Run("an action with no record yet resolves to empty copy, never an error", func(t *testing.T) {
		res, err := actionstate.New().Resolve(t.Context(), depsWith(t, nil), reqFor(t, viewer, "advance"))
		require.NoError(t, err, "a Tier-0 page must paint complete before anyone clicks anything")
		// The LITERAL, not NotEmpty and not another DisplayCopy call. NotEmpty
		// was satisfied by the diagnostic string this arm used to return
		// ("Unknown state."), which a Tier-0 page painted on first load; and
		// comparing against DisplayCopy("") would be satisfied by whatever
		// DisplayCopy happens to return, including that same diagnostic.
		assert.JSONEq(t, `""`, string(res.Value),
			"a control nobody has touched shows nothing, not a sentence about a state nothing reached")
	})

	t.Run("the newest record for the action wins", func(t *testing.T) {
		d := depsWith(t, []uiaction.Content{
			{RequestID: "r1", Action: "advance", State: uiaction.StateFailed, Requester: "viewer-1", UpdatedAt: older},
			{RequestID: "r2", Action: "advance", State: uiaction.StateRunning, Requester: "viewer-1", UpdatedAt: newer},
		})
		res, err := actionstate.New().Resolve(t.Context(), d, reqFor(t, viewer, "advance"))
		require.NoError(t, err)
		assert.JSONEq(t, quoted(uiaction.DisplayCopy(uiaction.StateRunning, false)), string(res.Value))
	})

	t.Run("another viewer's record is invisible", func(t *testing.T) {
		d := depsWith(t, []uiaction.Content{
			{RequestID: "r1", Action: "advance", State: uiaction.StateRunning, Requester: "viewer-2", UpdatedAt: newer},
		})
		res, err := actionstate.New().Resolve(t.Context(), d, reqFor(t, viewer, "advance"))
		require.NoError(t, err)
		assert.JSONEq(t, `""`, string(res.Value),
			"a shared session must not render one viewer's pending action on another's page")
	})

	t.Run("a record for a different action does not leak into this ref's resolution", func(t *testing.T) {
		// The viewer's OWN newest record is for "cancel", not "advance". If the
		// Action==req.Ref filter were ever dropped, resolving "advance" here
		// would incorrectly report cancel's StateRunning instead of falling
		// back to idle — a wrong-action bleed, distinct from the cross-viewer
		// leak the other cases cover.
		d := depsWith(t, []uiaction.Content{
			{RequestID: "r1", Action: "cancel", State: uiaction.StateRunning, Requester: "viewer-1", UpdatedAt: newer},
		})
		res, err := actionstate.New().Resolve(t.Context(), d, reqFor(t, viewer, "advance"))
		require.NoError(t, err)
		assert.JSONEq(t, `""`, string(res.Value),
			"a record for a DIFFERENT declared action must never answer THIS ref's binding")
	})

	t.Run("an approval addressed to the viewer reads differently than one addressed elsewhere", func(t *testing.T) {
		mine := resolveCopy(t, viewer, uiaction.Content{Action: "advance", State: uiaction.StateAwaitingApproval,
			Requester: "viewer-1", ApprovalAddressedToViewer: true, UpdatedAt: newer})
		theirs := resolveCopy(t, viewer, uiaction.Content{Action: "advance", State: uiaction.StateAwaitingApproval,
			Requester: "viewer-1", ApprovalAddressedToViewer: false, UpdatedAt: newer})
		assert.NotEqual(t, mine, theirs)
	})

	t.Run("a nil Memory fails closed with a returned error, never an empty value", func(t *testing.T) {
		_, err := actionstate.New().Resolve(t.Context(), depsWithNilMemory(t), reqFor(t, viewer, "advance"))
		assert.Error(t, err)
	})

	t.Run("a non-empty args template is refused at call time too", func(t *testing.T) {
		r := reqFor(t, viewer, "advance")
		r.Args = json.RawMessage(`{"anything":1}`)
		_, err := actionstate.New().Resolve(t.Context(), depsWith(t, nil), r)
		assert.Error(t, err, "the validator rejects this at write time; this is the call-time half")
	})

	t.Run("the returned copy leaks no internal identifier", func(t *testing.T) {
		got := resolveCopy(t, viewer, uiaction.Content{Action: "advance", State: uiaction.StateDenied,
			Requester: "viewer-1", RequestID: "req-abc123", UpdatedAt: newer})
		for _, banned := range []string{"req-abc123", "viewer-1", "ui_action", "AgentUI", "ap.session."} {
			assert.NotContains(t, got, banned)
		}
	})
}
