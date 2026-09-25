package memoryref_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/memoryref"
)

// appendOnlyKindName is a fixture Kind name — never a real audit kind's
// name — registered below with Retention().AppendOnly true, which is the
// only property the resolver's readability check actually inspects.
const appendOnlyKindName = "demo-audit"

// fixtureKind is a package-local memory.Kind used only to populate the real
// memory registry for this test, so memoryref's LookupKind/AppendOnly checks
// have something concrete to classify without depending on any production
// Kind's name or shape.
type fixtureKind struct {
	name       string
	prefix     string
	appendOnly bool
}

func (k fixtureKind) Name() string     { return k.name }
func (k fixtureKind) IDPrefix() string { return k.prefix }
func (k fixtureKind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: k.appendOnly}
}
func (fixtureKind) ContentSchema() reflect.Type { return nil }
func (fixtureKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (fixtureKind) WriteAuthority() memory.WriteAuthority        { return memory.SessionWritten }
func (fixtureKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return fixtureHooks{} }

type fixtureHooks struct{}

func (fixtureHooks) OnSignal(context.Context, memory.Signal) error { return nil }

func TestMain(m *testing.M) {
	memory.RegisterKind(fixtureKind{name: "note", prefix: "note-"})
	memory.RegisterKind(fixtureKind{name: appendOnlyKindName, prefix: "aud-", appendOnly: true})
	code := m.Run()
	memory.ResetRegistryForTest()
	os.Exit(code)
}

// fakeMemory records the memory.Query it receives so tests can assert on the
// exact scope/filters the resolver built.
type fakeMemory struct {
	got    memory.Query
	result memory.QueryResult
	err    error
}

func (f *fakeMemory) Put(context.Context, memory.Entry) (memory.Entry, error) {
	return memory.Entry{}, nil
}
func (f *fakeMemory) Query(_ context.Context, q memory.Query) (memory.QueryResult, error) {
	f.got = q
	return f.result, f.err
}
func (f *fakeMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (f *fakeMemory) SendSignal(context.Context, memory.Signal) error { return nil }

// fakeDeps' mem field is typed as the memory.Memory INTERFACE (not a
// pointer), so the zero value fakeDeps{} carries a genuine nil interface —
// exactly the case the resolver's fail-closed guard must catch without a
// typed-nil false negative.
type fakeDeps struct {
	mem memory.Memory
}

func (f fakeDeps) Memory() memory.Memory                                   { return f.mem }
func (f fakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (f fakeDeps) NATSRequest() channelevents.RequestFunc                  { return nil }
func (f fakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (f fakeDeps) Logger() logr.Logger                                     { return logr.Discard() }

func TestMemoryResolver(t *testing.T) {
	r := memoryref.New()
	assert.Equal(t, "memory", r.Source())

	t.Run("scope comes from the request, never the args", func(t *testing.T) {
		fake := &fakeMemory{result: memory.QueryResult{Entries: []memory.Entry{
			{Kind: "note", ID: "note-1", Content: json.RawMessage(`{"title":"a"}`)},
			{Kind: "note", ID: "note-2", Content: json.RawMessage(`{"title":"b"}`)},
		}}}
		res, err := r.Resolve(context.Background(), fakeDeps{mem: fake}, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Path: "root/#rows",
			Ref: "note", Args: json.RawMessage(`{"tags":["pinned"],"limit":10}`),
		})
		require.NoError(t, err)
		assert.Equal(t, memory.Scope{Kind: "session", ID: "demo-ns/demo-session"}, fake.got.Scope)
		assert.Equal(t, []string{"note"}, fake.got.Kinds)
		assert.Equal(t, []string{"pinned"}, fake.got.Tags)
		assert.Equal(t, 10, fake.got.Limit)
		assert.JSONEq(t, `[{"title":"a"},{"title":"b"}]`, string(res.Value),
			"the value is the entries' contents, bindable straight to ap:table rows")
	})

	t.Run("an args field that tries to redirect the scope is rejected, not ignored", func(t *testing.T) {
		_, err := r.Resolve(context.Background(), fakeDeps{mem: &fakeMemory{}}, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "note",
			Args: json.RawMessage(`{"scope":{"kind":"session","id":"other-ns/other-session"}}`),
		})
		require.Error(t, err)
	})

	errCases := []struct {
		name    string
		ref     string
		wantErr string
	}{
		{name: "an unregistered kind is refused", ref: "no-such-kind", wantErr: "not available"},
		{name: "an append-only audit kind is refused", ref: appendOnlyKindName, wantErr: "not available"},
		{name: "an empty ref is refused", ref: "", wantErr: "not available"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Resolve(context.Background(), fakeDeps{mem: &fakeMemory{}}, uibindings.Request{
				Namespace: "demo-ns", Session: "demo-session", Ref: tc.ref,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("a result over the UI ceiling is refused rather than streamed to the browser", func(t *testing.T) {
		big := json.RawMessage(`"` + strings.Repeat("x", int(toolguard.DefaultUIIngressBytes)+1) + `"`)
		fake := &fakeMemory{result: memory.QueryResult{Entries: []memory.Entry{{Kind: "note", Content: big}}}}
		_, err := r.Resolve(context.Background(), fakeDeps{mem: fake}, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "note",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too large")
	})

	t.Run("a nil Memory fails closed with a returned error", func(t *testing.T) {
		_, err := r.Resolve(context.Background(), fakeDeps{}, uibindings.Request{
			Namespace: "demo-ns", Session: "demo-session", Ref: "note",
		})
		require.Error(t, err)
	})
}
