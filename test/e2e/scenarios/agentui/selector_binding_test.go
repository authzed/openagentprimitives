//go:build e2e

// selector_binding_test.go is the proof that the whole selector path works
// against a REALISTICALLY-shaped tool, through the real runner. Every other
// scenario in this package (leads_console_*, view_capability_test.go,
// browserarm_test.go) drives pkg/web/uidemo/leadflow tools whose response shape
// was written to match a component's props exactly — that circularity is
// exactly what plan 8 exists to correct. leadflow.ToolSearchAccounts answers with an ENVELOPE
// ({"results":[…],"total":…,"paging":{…}}, each record's fields nested one
// level down under "properties") the way a real third-party CRM search
// would, and this file is the only place a component binds through
// pkg/web/uiselect against that shape end to end: the real bindingsHandler
// (pkg/web/webui/agentui/bindings.go), the real "tool" uibindings.Resolver
// (pkg/web/uibindings/tool), the real runner.Loop.HandleUIDataBinding readonly
// gate and per-origin rate limit, and the real
// pkg/web/uibindings/tool.unwrapResult — which only fires on a genuine MCP
// round-trip (the Result field is JSON text, never a Go value directly), a
// composition no fake resolver in this package's sibling scenarios exercises.
package agentui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// selectorBindingGrantedTools is this fixture's AgentClass.spec.agentUI's
// own grantedTools list (03-agentclass.yaml) — passed to stampAgentUIValidity
// so the two stay in sync without a second source of truth, matching
// leads-console-e2e's pipelineDeskGrantedTools convention.
var selectorBindingGrantedTools = []string{"acct_search_accounts"}

// applySelectorBindingFixture applies every *.yaml file in
// test/e2e/testdata/selector-binding-e2e (sorted by the 00-/01-/… prefix
// order that encodes apply order), substituting {{CRM_URL}} with crmURL.
//
// Deliberately its own function rather than a reuse of browserarm_test.go's
// applyLeadsConsoleFixture: that helper hardcodes the "leads-console-e2e"
// directory, and this scenario intentionally does NOT extend that shared
// fixture (four other test files in this package already depend on its
// exact tool/grant shape) — see 01-mcpserver.yaml's own doc comment for why
// a dedicated fixture, not a fourth tool bolted onto the shared one, is the
// lower-risk choice here. The loop body is otherwise identical.
func applySelectorBindingFixture(t *testing.T, h *e2e.Harness, crmURL string) {
	t.Helper()
	dir := e2e.TestdataDir("selector-binding-e2e")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read selector-binding-e2e fixture dir")

	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, "read fixture file %s", name)
		substituted := strings.ReplaceAll(string(raw), "{{CRM_URL}}", crmURL)
		h.ApplyManifest(substituted)
	}
}

// startSelectorBindingSession stands up the harness, a real leadflow
// server, applies the fixture, stamps AgentUI validity, waits for the
// AgentClass and SpiceDB bootstrap, drives the mandatory channel-attached
// cold-start turn (see 05-channel.yaml's doc comment for why one is
// unavoidable), and returns the harness, namespace, and session name a
// subtest binds against. Shared by both tests below so their fixture setup
// cannot silently drift apart.
func startSelectorBindingSession(t *testing.T) (h *e2e.Harness, ns, sessionName string) {
	t.Helper()
	_, crm := newLeadflowServer(t)
	h = e2e.Start(t, e2e.Options{})
	applySelectorBindingFixture(t, h, crm.URL)
	stampAgentUIValidity(t, h, "default", "acctbook-ui", selectorBindingGrantedTools)
	h.WaitForAgentClassValid("acctbook-desk", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	const viewerEmail = "viewer@demo.invalid"

	// Repeating(): a spec.Prompt replay can drive the seed message through
	// the LLM more than once (see leads_console_read_test.go's identical
	// note); agent_work_complete needs no respond_to_user round-trip to
	// reach Idle.
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "opened account search"})).
		Repeating()

	h.SendUserMessage("open account search", e2e.AsUser(viewerEmail))
	e2e.WaitForSessionIdle(t, h)

	sess := e2e.FindOnlyAgentSession(t, h, h.Namespace())
	return h, h.Namespace(), sess.Name
}

// TestSelectorBinding_ExtractsThroughTheRealRunner asserts the EXTRACTED
// content, not merely a non-empty response — the whole failure mode plan 8
// corrects is "the binding returns something that isn't what the component
// needs". A binding with no selector would resolve to the raw
// SearchAccountsEnvelope object ({"results":…,"total":…,"paging":…}); this
// asserts the selected value is instead the ROWS ap:table needs: each
// element carrying name/stage at the top level and none of the envelope's
// own keys anywhere in it.
func TestSelectorBinding_ExtractsThroughTheRealRunner(t *testing.T) {
	h, ns, sessionName := startSelectorBindingSession(t)

	viewer := e2e.CanonicalForFakeEmail("viewer@demo.invalid")
	b := h.AgentUIBrowserFor("user:" + viewer.String())
	ctx := context.Background()

	got, status, err := b.Bindings(ctx, ns, sessionName, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	// accounts is not agentWritable and compiles to no hook (region ""); it
	// is the root stack's first child, whose ap:table (child 0) carries rows.
	res, ok := got["/0.0#rows"]
	require.True(t, ok, "expected the accounts slot's ap:table rows binding in the response")
	require.Equal(t, "ok", res.Status, "binding did not resolve: %s", res.Message)

	var rows []map[string]any
	require.NoError(t, json.Unmarshal(res.Value, &rows), "the extracted value must be a JSON array of rows, not the envelope object")
	require.Len(t, rows, 12, "the seeded book's full account list — see pkg/web/uidemo/leadflow/seed.go")

	for i, row := range rows {
		_, hasName := row["name"]
		assert.True(t, hasName, "row %d must carry AccountProperties.name at the top level", i)
		_, hasStage := row["stage"]
		assert.True(t, hasStage, "row %d must carry AccountProperties.stage at the top level", i)
		for _, envelopeKey := range []string{"results", "total", "paging", "properties"} {
			_, present := row[envelopeKey]
			assert.False(t, present, "row %d must not carry the envelope's own %q key — select must have unwrapped it", i, envelopeKey)
		}
	}
}

// TestSelectorBinding_AMissingPathFailsThatBindingOnly is the containment
// claim ("one bad selector costs one section, never the page") asserted
// against the REAL fan-out (pkg/web/webui/agentui/bindings.go's concurrent
// resolveOneBinding calls) rather than a fake. accountsMiss binds the SAME
// tool with select "items[].properties" — the envelope's array lives under
// "results", not "items" — so that binding must fail while accounts, in
// the SAME request, still resolves.
func TestSelectorBinding_AMissingPathFailsThatBindingOnly(t *testing.T) {
	h, ns, sessionName := startSelectorBindingSession(t)

	viewer := e2e.CanonicalForFakeEmail("viewer@demo.invalid")
	b := h.AgentUIBrowserFor("user:" + viewer.String())
	ctx := context.Background()

	got, status, err := b.Bindings(ctx, ns, sessionName, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "a per-binding failure must never fail the whole request")

	// Neither slot is agentWritable, so neither compiles to a hook: accounts
	// is root child 0, accountsMiss is root child 1, each carrying its
	// ap:table's rows binding at index 0.
	good, ok := got["/0.0#rows"]
	require.True(t, ok, "expected the accounts slot's ap:table rows binding in the response")
	assert.Equal(t, "ok", good.Status, "the good selector must still resolve: %s", good.Message)
	assert.NotEmpty(t, good.Value, "the good binding's value must not be dropped by its sibling's failure")

	bad, ok := got["/1.0#rows"]
	require.True(t, ok, "expected the accountsMiss slot's ap:table rows binding in the response")
	assert.Equal(t, "error", bad.Status, "a selector naming a path the envelope does not have must fail its OWN binding")
	assert.NotEmpty(t, bad.Message, "a failed binding must carry viewer-safe copy explaining the outcome")
}
