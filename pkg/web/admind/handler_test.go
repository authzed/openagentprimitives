package admind_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/audit"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// stubChecker scripts platform-permission outcomes per canonical subject and a
// scripted platform-admin listing.
type stubChecker struct {
	allow     map[string]bool // canonicalID → allowed (all permissions)
	err       error
	admins    []string // scripted ListPlatformAdmins result
	adminsErr error    // scripted ListPlatformAdmins error
}

func (s stubChecker) CheckPlatformPermission(_ context.Context, _ string, canonicalID identity.CanonicalUserID, _ bool) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.allow[canonicalID.String()], nil
}

func (s stubChecker) ListPlatformAdmins(_ context.Context) ([]string, error) {
	if s.adminsErr != nil {
		return nil, s.adminsErr
	}
	return s.admins, nil
}

// CheckAgentIdentityUpdateCredential completes PlatformChecker for the
// per-resource credential-replacement route. It denies unconditionally: no test
// in this file exercises that route (credentials_test.go has its own
// call-counting checker), and a stub that silently ALLOWED would be a
// fail-open default sitting in the fixture every future test picks up.
func (s stubChecker) CheckAgentIdentityUpdateCredential(_ context.Context, _, _ string, _ identity.CanonicalUserID) (bool, error) {
	return false, nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

func newTestAdmind(t *testing.T, k8s client.Client) *admind.Admind {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	a, err := admind.New(admind.Config{
		Mem:     mem,
		K8s:     k8s,
		Checker: stubChecker{allow: map[string]bool{"YWRtaW4": true}},
		Token:   "test-token",
		Logger:  testr.New(t),
		// Point the GKE metadata lookup at a closed loopback port so it fails
		// fast (connection refused) → the cluster detection falls back to the
		// node-name heuristic without a real (slow) network call. A test that
		// exercises the metadata path builds its own Admind with a live server.
		MetadataBaseURL: "http://127.0.0.1:1",
	})
	require.NoError(t, err)
	return a
}

func do(t *testing.T, h http.Handler, method, path, token, subject string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if subject != "" {
		req.Header.Set("X-Admin-Subject", subject)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAdmindAuth(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	h := a.Handler()

	cases := []struct {
		name           string
		token, subject string
		want           int
	}{
		{"missing token → 401", "", "user:YWRtaW4", http.StatusUnauthorized},
		{"wrong token → 401", "nope", "user:YWRtaW4", http.StatusUnauthorized},
		{"missing subject → 401", "test-token", "", http.StatusUnauthorized},
		{"subject without the user: prefix → 401", "test-token", "YWRtaW4", http.StatusUnauthorized},
		{"bare user: prefix with no canonical id → 401", "test-token", "user:", http.StatusUnauthorized},
		{"non-admin subject → 403", "test-token", "user:bm9wZQ", http.StatusForbidden},
		{"admin subject → 200", "test-token", "user:YWRtaW4", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, http.MethodGet, "/admin/v1/sessions", tc.token, tc.subject, "")
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

func TestAdmindAuth_SpiceDBErrorIs500(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	a, err := admind.New(admind.Config{
		Mem: mem, K8s: fake.NewClientBuilder().WithScheme(newScheme(t)).Build(),
		Checker: stubChecker{err: context.DeadlineExceeded},
		Token:   "test-token", Logger: testr.New(t),
	})
	require.NoError(t, err)
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "SpiceDB error must NOT read as denied")
}

func TestAdmindSessionsAndKill(t *testing.T) {
	sess := sessionCR("default", "s1", "support-bot", "Running")
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)
	h := a.Handler()

	w := do(t, h, http.MethodGet, "/admin/v1/sessions", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var list []admind.SessionState
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.Equal(t, "s1", list[0].Name)

	w = do(t, h, http.MethodGet, "/admin/v1/sessions/default/s1", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	w = do(t, h, http.MethodDelete, "/admin/v1/sessions/default/s1", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusNoContent, w.Code)
	// CR really deleted:
	var got spiceboxv1alpha1.AgentSession
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "s1"}, &got)
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "AgentSession must be deleted")

	w = do(t, h, http.MethodDelete, "/admin/v1/sessions/default/s1", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "second delete → 404")
}

func TestAdmindApprovals(t *testing.T) {
	sess := sessionCR("default", "s1", "support-bot", "Running")
	// Both approval families come from the single generic PendingInteractions
	// list since Slice C2; Tool shows the publisher's Summary.
	sess.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
		{RequestID: "tg1", Category: categories.ToolApproval, Summary: "code_gh"},
		{RequestID: "lk1", Category: categories.InfoLeakage, Summary: "share crm export"},
	}
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)
	h := a.Handler()

	// view_live gates it: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/approvals", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "approvals needs view_live")

	w = do(t, h, http.MethodGet, "/admin/v1/approvals", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var apps []admind.Approval
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &apps))
	require.Len(t, apps, 2)
	kinds := map[string]admind.Approval{}
	for _, ap := range apps {
		kinds[ap.Kind] = ap
	}
	assert.Equal(t, "code_gh", kinds["tool_call"].Tool)
	assert.Equal(t, "share crm export", kinds["leakage"].Tool)
	assert.Empty(t, kinds["tool_call"].Requester, "the generic list carries no requester")
	assert.Empty(t, kinds["leakage"].Requester, "the generic list carries no requester")
	assert.Equal(t, "support-bot", kinds["tool_call"].Class)
}

func TestAdmindAuditEndpoints(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	// Seed one audit entry through the same facade the engine queries.
	_, err := a.Memory().Put(context.Background(), entryFor(t))
	require.NoError(t, err)
	h := a.Handler()

	w := do(t, h, http.MethodPost, "/admin/v1/audit/query", "test-token", "user:YWRtaW4", `{"kinds":["approval"]}`)
	require.Equal(t, http.StatusOK, w.Code)
	var qr audit.QueryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &qr))
	require.Len(t, qr.Events, 1)

	w = do(t, h, http.MethodPost, "/admin/v1/audit/facets", "test-token", "user:YWRtaW4", `{}`)
	require.Equal(t, http.StatusOK, w.Code)

	w = do(t, h, http.MethodGet, "/admin/v1/audit/entities/sessions", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	w = do(t, h, http.MethodGet, "/admin/v1/audit/entities/bogus", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAdmindOverviewAndHealth(t *testing.T) {
	sess := sessionCR("default", "s1", "support-bot", "Running")
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)
	h := a.Handler()

	// view_overview is required: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/overview", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "overview needs view_overview")

	// Overview JSON shape: kpis + 24-bucket series + budget(estimated).
	w = do(t, h, http.MethodGet, "/admin/v1/overview", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var ov struct {
		ByModel   []map[string]any `json:"byModel"`
		Series24h []map[string]any `json:"series24h"`
		KPIs      map[string]any   `json:"kpis"`
		Budget    struct {
			TokensSpent      int64   `json:"tokensSpent"`
			EstimatedCostUSD float64 `json:"estimatedCostUSD"`
			Estimated        bool    `json:"estimated"`
		} `json:"budget"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ov))
	assert.Len(t, ov.Series24h, 24, "24 hourly buckets")
	assert.Contains(t, ov.KPIs, "activeSessions")
	assert.True(t, ov.Budget.Estimated, "budget cost is flagged estimated")

	// Health JSON shape: a component list with the first-party workloads.
	w = do(t, h, http.MethodGet, "/admin/v1/health", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var hr struct {
		Components []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"components"`
		Rollup map[string]any `json:"rollup"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &hr))
	require.NotEmpty(t, hr.Components, "health reports first-party components")
	names := make(map[string]bool, len(hr.Components))
	for _, c := range hr.Components {
		names[c.Name] = true
	}
	assert.True(t, names["operator"], "operator component present")
	assert.True(t, names["graphiti"], "graphiti component present (NotConfigured w/o endpoint)")
}

func entryFor(t *testing.T) memory.Entry {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"toolName": "bash", "decision": "approved", "approver": "user:abc"})
	require.NoError(t, err)
	return memory.Entry{Scope: memory.Scope{Kind: "session", ID: "default/s1"},
		Kind: "approval", ID: "approval-1", Content: raw}
}
