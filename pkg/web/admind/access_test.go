package admind_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// newAccessAdmind builds an Admind whose Checker scripts both the auth allow
// map and a platform-admin listing.
func newAccessAdmind(t *testing.T, admins []string) *admind.Admind {
	t.Helper()
	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     fake.NewClientBuilder().WithScheme(newScheme(t)).Build(),
		Checker: stubChecker{allow: map[string]bool{"YWRtaW4": true}, admins: admins},
		Token:   "test-token",
		Logger:  testr.New(t),
	})
	require.NoError(t, err)
	return a
}

func TestAdmindAccess(t *testing.T) {
	a := newAccessAdmind(t, []string{"user:YWRtaW4", "group:platform-admins#member"})
	h := a.Handler()

	// view_config gates it: a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/access", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "access needs view_config")

	w = do(t, h, http.MethodGet, "/admin/v1/access", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Admins []struct {
			Subject string `json:"subject"`
			Kind    string `json:"kind"`
		} `json:"admins"`
		GrantCmd string `json:"grantCmd"`
		Stats    struct {
			SchemaDefs    *int `json:"schemaDefs"`
			Relationships *int `json:"relationships"`
			Available     bool `json:"available"`
		} `json:"stats"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Admins, 2)
	byKind := map[string]string{} // kind → subject
	for _, ad := range resp.Admins {
		byKind[ad.Kind] = ad.Subject
	}
	assert.Equal(t, "user:YWRtaW4", byKind["user"], "user subject classified user")
	assert.Equal(t, "group:platform-admins#member", byKind["group"], "group subject classified group")

	assert.Equal(t, "oap platform grant-admin <email>", resp.GrantCmd)

	// SpiceDB schema/relationship stats are not plumbed for v1: degraded panel.
	assert.False(t, resp.Stats.Available, "stats reported unavailable, not errored")
	assert.Nil(t, resp.Stats.SchemaDefs)
	assert.Nil(t, resp.Stats.Relationships)
}

func TestAdmindAccess_ListError500(t *testing.T) {
	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     fake.NewClientBuilder().WithScheme(newScheme(t)).Build(),
		Checker: stubChecker{allow: map[string]bool{"YWRtaW4": true}, adminsErr: assertErr{}},
		Token:   "test-token",
		Logger:  testr.New(t),
	})
	require.NoError(t, err)
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/access", "test-token", "user:YWRtaW4", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "ListPlatformAdmins error → 500")
}

type assertErr struct{}

func (assertErr) Error() string { return "list admins boom" }
