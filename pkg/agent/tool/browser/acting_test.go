package browser

import (
	"context"
	"net/http"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// actingTool builds one acting browserTool whose Session reports host as its
// current page host, without any Chrome — enough to exercise Permission and
// NamedArgs in isolation.
func actingTool(op, host string) *browserTool {
	return &browserTool{op: op, sess: &Session{currentHost: func() string { return host }}}
}

// TestActingOpPermissionIsExternalWebHost proves each acting op authorizes
// External on the web_host slot with the act permission, and an observation op
// stays Passthrough.
func TestActingOpPermissionIsExternalWebHost(t *testing.T) {
	for _, op := range []string{"click", "press", "select", "submit"} {
		t.Run(op+": External on web_host#act", func(t *testing.T) {
			p := actingTool(op, "aa.com").Permission()
			assert.Equal(t, authz.External, p.StateImpact)
			require.NotNil(t, p.Check, "an acting op must carry a Check")
			assert.Equal(t, browserschema.WebHostType, p.Check.ResourceType)
			assert.Equal(t, browserschema.ActPermission, p.Check.Permission)
			assert.Equal(t, "{"+hostArgKey+"}", p.Check.ResourceIDTemplate)
			assert.Empty(t, p.Check.ResourceIDTransforms,
				"the canonical host is a valid object id as-is; no transforms needed")
		})
	}

	t.Run("read: Passthrough with no Check", func(t *testing.T) {
		p := actingTool("read", "aa.com").Permission()
		assert.Equal(t, authz.Passthrough, p.StateImpact)
		assert.Nil(t, p.Check, "an observation op maps to no SpiceDB resource")
	})
}

// TestActingNamedArgsSurfacesPageHost proves the page host reaches the check's
// ResolveResourceID under hostArgKey, yielding the canonical host — and that it
// is PAGE STATE: a model-supplied "host" cannot override the real one.
func TestActingNamedArgsSurfacesPageHost(t *testing.T) {
	bt := actingTool("click", "aa.com")

	named, ok := bt.NamedArgs(map[string]any{"ref": "e3", "host": "attacker.example"})
	require.True(t, ok)
	assert.Equal(t, "aa.com", named[hostArgKey],
		"the live page host must win over a host the model put in the call")
	assert.Equal(t, "e3", named["ref"], "other args pass through untouched")

	id, err := authz.ResolveResourceID(*bt.Permission().Check, named)
	require.NoError(t, err)
	assert.Equal(t, "aa.com", id, "the check resolves the instance to the canonical page host")
}

// TestObservationNamedArgsPassThrough proves an observation op does not touch the
// args (no host surfaced, no live-location round-trip on the authz path).
func TestObservationNamedArgsPassThrough(t *testing.T) {
	bt := &browserTool{op: "read", sess: &Session{currentHost: func() string {
		t.Fatal("CurrentHost must not be read for an observation op")
		return ""
	}}}
	in := map[string]any{"page": 2}
	out, ok := bt.NamedArgs(in)
	require.True(t, ok)
	assert.Equal(t, in, out)
	_, hasHost := out[hostArgKey]
	assert.False(t, hasHost, "an observation op surfaces no host")
}

// recordingCli is a toolcheck.Client that records whether SpiceDB was queried,
// so the empty-host test can prove the denial happens BEFORE any round trip.
type recordingCli struct{ called bool }

func (r *recordingCli) CheckPermission(context.Context, *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	r.called = true
	return &v1.CheckPermissionResponse{Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}, nil
}

// TestActingEmptyHostFailsClosed proves an empty current host (about:blank / no
// page) is an UnresolvedResource denial routed to a human — never a silent allow,
// and never a wasted SpiceDB query.
func TestActingEmptyHostFailsClosed(t *testing.T) {
	bt := actingTool("click", "") // CurrentHost resolves to ""

	named, ok := bt.NamedArgs(map[string]any{"ref": "e3"})
	require.True(t, ok)
	assert.Equal(t, "", named[hostArgKey])

	cli := &recordingCli{}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), bt.Permission(), authz.Inputs{
		Subject:           "u-1",
		Args:              named,
		AgentSessionRef:   "ns/s1",
		SlotResourceTypes: []string{browserschema.WebHostType},
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "empty host must deny")
	assert.True(t, res.UnresolvedResource, "empty host must be an UnresolvedResource denial, never a silent allow")
	assert.False(t, cli.called, "an unresolved host must deny before querying SpiceDB")
}

// --- chrome-backed DOM-mutation tests (local Chrome via startFixture) ---
//
// These drive a real headless Chrome through the in-process gate and assert the
// acting op actually changed the page. The testbrowser C10 login site is not
// built yet, so each test serves its own minimal form/handler page from the
// httptest site the fixture allowlists.

func TestClickMutatesDOM(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<button onclick="document.getElementById('out').textContent='clicked'">Go</button>` +
			`<p id="out">idle</p></body></html>`))
	}))
	fx.rt.Spec.Interaction = "act"
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	btnRef := refForRole(t, read.Content, "button")

	clicked := run(t, toolBy(t, tools, "click"), map[string]any{"ref": btnRef})
	assert.False(t, clicked.IsError, "click: %s", clicked.Content)

	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError, "read after click: %s", after.Content)
	assert.Contains(t, after.Content, "clicked", "click must fire the element's onclick handler")
}

func TestSelectMutatesDOM(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<select aria-label="fruit" onchange="document.getElementById('out').textContent='picked:'+this.value">` +
			`<option value="a">Apple</option><option value="b">Banana</option></select>` +
			`<p id="out">idle</p></body></html>`))
	}))
	fx.rt.Spec.Interaction = "act"
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	selRef := refForRole(t, read.Content, "textbox") // <select> renders combobox→textbox

	sel := run(t, toolBy(t, tools, "select"), map[string]any{"ref": selRef, "option": "Banana"})
	assert.False(t, sel.IsError, "select: %s", sel.Content)

	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError, "read after select: %s", after.Content)
	assert.Contains(t, after.Content, "picked:b", "select must set the value and fire change")
}

func TestSubmitMutatesDOM(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<form onsubmit="document.getElementById('out').textContent='submitted';return false">` +
			`<input type="text" aria-label="q"><button type="submit">Go</button></form>` +
			`<p id="out">idle</p></body></html>`))
	}))
	fx.rt.Spec.Interaction = "act"
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	read := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, read.IsError, "read: %s", read.Content)
	btnRef := refForRole(t, read.Content, "button")

	sub := run(t, toolBy(t, tools, "submit"), map[string]any{"ref": btnRef})
	assert.False(t, sub.IsError, "submit: %s", sub.Content)

	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError, "read after submit: %s", after.Content)
	assert.Contains(t, after.Content, "submitted", "submit must trigger the form's onsubmit")
}

func TestPressDispatchesKey(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>` +
			`<script>document.addEventListener('keydown',function(e){document.getElementById('out').textContent='key:'+e.key});</script>` +
			`<p id="out">idle</p></body></html>`))
	}))
	fx.rt.Spec.Interaction = "act"
	tools := fx.synthTools(t, false)
	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)

	pressed := run(t, toolBy(t, tools, "press"), map[string]any{"key": "a"})
	assert.False(t, pressed.IsError, "press: %s", pressed.Content)

	after := run(t, toolBy(t, tools, "read"), map[string]any{})
	require.False(t, after.IsError, "read after press: %s", after.Content)
	assert.Contains(t, after.Content, "key:a", "press must dispatch a keydown the page observes")
}

// TestCurrentHostReadsLivePage proves CurrentHost reads and canonicalizes the
// real page host, and is "" before any navigation (about:blank) — the fail-closed
// instance the acting check denies on.
func TestCurrentHostReadsLivePage(t *testing.T) {
	fx := startFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Home</h1></body></html>`))
	}))
	fx.rt.Spec.Interaction = "act"
	tools := fx.synthTools(t, false)
	bt, ok := tools[0].(*browserTool)
	require.True(t, ok)

	assert.Equal(t, "", bt.sess.CurrentHost(), "no page yet (about:blank) has no host")

	require.False(t, run(t, toolBy(t, tools, "navigate"), map[string]any{"url": fx.siteBase + "/"}).IsError)
	assert.Equal(t, "site.test", bt.sess.CurrentHost(), "CurrentHost returns the canonical live host")
}
