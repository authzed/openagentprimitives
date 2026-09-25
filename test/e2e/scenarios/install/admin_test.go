//go:build e2e

package install_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/audit"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestAdminAPI_EndToEnd drives the real stack: a ping/pong conversation
// creates an AgentSession + memory entries; admind (token-gated, real
// SpiceDB platform check) lists the session, returns its audit events,
// and kills it.
func TestAdminAPI_EndToEnd(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: e2e.TestdataDir("agent-centerdot-companies")})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)
	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong")).Repeating()
	h.SendUserMessage("ping")
	h.ExpectAgentReply(e2e.Contains("pong"))

	ctx := context.Background()

	// Grant the admin in real SpiceDB. The Guardian compose is async, but the
	// barrier for it is already held: WaitForAgentClassValid above cannot
	// return until validateAgainstSchema resolved this class's MCPServer
	// permissions against the LIVE schema, which only exists after a Guardian
	// WriteSchema — and every composed schema is the code-owned scaffold
	// (pkg/authz/spicedb/schema/schema.zed, where `definition platform` lives) with
	// fragments concatenated onto it. So `platform` is present by construction
	// here, and this is a hard assertion rather than a poll.
	//
	// Deliberately NOT wrapped in eventuallyNoError. Measured over repeated
	// runs it succeeded on the first attempt every time, so a retry could only
	// ever hide a regression in that ordering behind a silent 15s wait. The
	// sibling guard in credential_update_agent_owned's grantPlatformAdmin IS a
	// retry, correctly: it runs BEFORE that package's WaitForAgentClassValid,
	// so there the barrier genuinely is not held yet.
	const adminCanonical = "YWxpY2VAZXhhbXBsZS5jb20"
	require.NoError(t, h.SpiceDB.TouchPlatformAdmin(ctx, identity.CanonicalFromTrusted(adminCanonical, "test fixture")),
		"the Guardian compose must already have put the base schema (with `definition platform`) "+
			"into SpiceDB by the time the AgentClass reports Valid")

	adm, err := admind.New(admind.Config{
		Mem:     h.Memory(),
		K8s:     h.K8s,
		Checker: h.SpiceDB,
		Token:   "e2e-admind-token",
		Logger:  testr.New(t),
	})
	require.NoError(t, err)

	// Feed the aggregator from the live CR list (the operator wires an
	// informer; the harness drives it directly).
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessions))
	require.NotEmpty(t, sessions.Items, "the conversation must have created a session")
	for i := range sessions.Items {
		adm.Aggregator().UpsertSession(&sessions.Items[i])
	}
	sess := sessions.Items[0]

	srv := httptest.NewServer(adm.Handler())
	t.Cleanup(srv.Close)

	call := func(method, path, subject, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer e2e-admind-token")
		req.Header.Set("X-Admin-Subject", subject)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		return resp
	}

	// Live list (real SpiceDB check on the forwarded subject). admind uses
	// snapshot (non-fully-consistent) reads, so SpiceDB's cached snapshot
	// may lag the freshly-written relationship by a brief window. Poll until
	// the admin subject sees the sessions — usually one or two retries.
	var list []admind.SessionState
	require.Eventually(t, func() bool {
		resp := call(http.MethodGet, "/admin/v1/sessions", "user:"+adminCanonical, "")
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			return false
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Logf("admin session list decode: %v", err)
			return false
		}
		return true
	}, 10*time.Second, 250*time.Millisecond, "admin subject must get 200 from session list")
	require.Len(t, list, len(sessions.Items))

	// Non-admin is denied by the REAL platform check.
	denyResp := call(http.MethodGet, "/admin/v1/sessions", "user:Ym9iQGV4YW1wbGUuY29t", "")
	// Security assertion: a non-admin must be denied by the REAL SpiceDB
	// platform check. require (not assert) so an authz regression fails fast
	// rather than continuing to kill the session under a wrong-state run.
	require.Equal(t, http.StatusForbidden, denyResp.StatusCode)
	require.NoError(t, denyResp.Body.Close())

	// Audit: the conversation produced turn entries → tool_call events
	// (respond_to_user) and lifecycle events.
	auditResp := call(http.MethodPost, "/admin/v1/audit/query", "user:"+adminCanonical, `{}`)
	require.Equal(t, http.StatusOK, auditResp.StatusCode)
	var qr audit.QueryResponse
	require.NoError(t, json.NewDecoder(auditResp.Body).Decode(&qr))
	require.NoError(t, auditResp.Body.Close())
	assert.NotEmpty(t, qr.Events, "conversation must have produced audit events")

	// ---- Part 7 surfaces ----
	// Each Part 7 admin route runs the SAME real two-factor check (service
	// token + SpiceDB platform permission) on a DIFFERENT per-area permission
	// (view_overview / view_live / view_audit / view_config). The admin grant
	// has already propagated — the live-list Eventually above converged on
	// view_sessions, and every Part 7 permission derives from the same
	// platform:platform#admin membership — so these are synchronous 200 reads.
	adminSubject := "user:" + adminCanonical
	getJSON := func(path string, into any) {
		t.Helper()
		resp := call(http.MethodGet, path, adminSubject, "")
		defer resp.Body.Close() //nolint:errcheck
		require.Equal(t, http.StatusOK, resp.StatusCode, "admin GET %s must be 200", path)
		if into != nil {
			require.NoError(t, json.NewDecoder(resp.Body).Decode(into), "decode %s", path)
		}
	}

	// Overview (view_overview): the kpis object is present.
	var ovResp map[string]json.RawMessage
	getJSON("/admin/v1/overview", &ovResp)
	require.Contains(t, ovResp, "kpis", "overview must carry the kpis object")

	// Health (view_overview): a component list (graphiti is NotConfigured in
	// e2e — that's a clean degraded state, not a failure).
	var healthResp struct {
		Components []map[string]any `json:"components"`
	}
	getJSON("/admin/v1/health", &healthResp)
	assert.NotEmpty(t, healthResp.Components, "health reports first-party components")

	// Live tool-calls + approvals (view_live): JSON arrays (may be empty — the
	// ping/pong used no gated tools and triggered no approvals).
	var toolCalls, approvals []map[string]any
	getJSON("/admin/v1/toolcalls", &toolCalls)
	getJSON("/admin/v1/approvals", &approvals)

	// Config / agents (view_config): the seeded centerdot-companies AgentClass
	// must appear in the projected rows — proves the projector path against a
	// real CR, not just an empty 200.
	var agentRows []struct {
		Name string `json:"name"`
	}
	getJSON("/admin/v1/config/agents", &agentRows)
	foundAgent := false
	for _, row := range agentRows {
		if row.Name == "centerdot-companies" {
			foundAgent = true
		}
	}
	assert.True(t, foundAgent, "config/agents must list the seeded centerdot-companies AgentClass")

	// Config / settings (view_config): rows, or the not-found note when there
	// is no ClusterAgentSettings (the e2e harness seeds none) — never a 404/500.
	var settingsResp struct {
		Rows []map[string]any `json:"rows"`
		Note string           `json:"note"`
	}
	getJSON("/admin/v1/config/settings", &settingsResp)

	// Access (view_config): the just-granted admin subject appears in admins.
	var accessResp struct {
		Admins []struct {
			Subject string `json:"subject"`
		} `json:"admins"`
	}
	getJSON("/admin/v1/access", &accessResp)
	foundAdmin := false
	for _, ad := range accessResp.Admins {
		if ad.Subject == adminSubject {
			foundAdmin = true
		}
	}
	assert.True(t, foundAdmin, "access must list the granted admin subject %q", adminSubject)

	// Memory (view_audit): the conversation wrote session-scoped append-only
	// entries (the same ones the audit query above returned), so the cross-scope
	// rollup is non-empty.
	var memResp struct {
		Rollups []map[string]any `json:"rollups"`
	}
	getJSON("/admin/v1/memory", &memResp)
	assert.NotEmpty(t, memResp.Rollups, "conversation must have produced memory rollups")

	// KG communities (view_audit): graphiti is off in e2e, so this degrades to
	// available:false — a 200, NEVER a 500. Asserting the flag is present proves
	// the degrade path, not an error page.
	var kgResp map[string]json.RawMessage
	getJSON("/admin/v1/kg/communities", &kgResp)
	require.Contains(t, kgResp, "available", "kg communities must degrade to available:false (200, not 500)")

	// Per-area denial on a NEW area: the non-admin is denied on view_overview,
	// proving per-permission gating rather than just view_sessions. require (not
	// assert) so an authz regression fails fast.
	denyOverview := call(http.MethodGet, "/admin/v1/overview", "user:Ym9iQGV4YW1wbGUuY29t", "")
	require.Equal(t, http.StatusForbidden, denyOverview.StatusCode, "non-admin must be denied on overview")
	require.NoError(t, denyOverview.Body.Close())

	// Kill: deletes the CR for real. The AgentSession controller's finalizer
	// runs asynchronously (scope cleanup), so we poll until the object is
	// gone rather than doing a one-shot Get.
	killResp := call(http.MethodDelete, "/admin/v1/sessions/"+sess.Namespace+"/"+sess.Name, "user:"+adminCanonical, "")
	assert.Equal(t, http.StatusNoContent, killResp.StatusCode)
	require.NoError(t, killResp.Body.Close())
	eventuallyNoError(t, 15*time.Second, func() error {
		var gone spiceboxv1alpha1.AgentSession
		err := h.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}, &gone)
		if err == nil {
			return fmt.Errorf("AgentSession still present (DeletionTimestamp: %v)", gone.DeletionTimestamp)
		}
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("unexpected Get error: %w", err)
		}
		return nil // NotFound — truly gone
	}, "AgentSession CR must be deleted after kill")
}

// eventuallyNoError polls fn every 100ms until it returns nil or the timeout
// elapses. Its one remaining caller waits on the AgentSession finalizer, a
// genuinely asynchronous actor: measured over repeated runs the first attempt
// always finds the CR still present and the second finds it gone. Mirrors the
// e2e.Eventually() helper in leakage_test.go but accepts a func() error.
func eventuallyNoError(t *testing.T, timeout time.Duration, fn func() error, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("eventuallyNoError: %s (last error: %v, timed out after %s)", msg, last, timeout)
}
