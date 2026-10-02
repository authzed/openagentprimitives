package admind_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// --- fixtures ---------------------------------------------------------------

const (
	tkToken    = "test-admind-token"
	tkAdmin    = "user:demo-admin" // holds view_tokens / revoke_token
	tkNonAdmin = "user:nobody"     // holds nothing
	tkAdminID  = "demo-admin"
	tkNS       = "agentprimitives-system"

	tkFullName    = "at-full0000001"
	tkFullOwner   = "alice"
	tkFullHash    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 64 hex chars
	tkRevokedName = "at-revoked0001"
	tkRevokedHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// stubGrantReader scripts ReadAccessTokenGrant per token id: a grant in
// `grants` returns found=true with that value; a tokenID in `errs` returns an
// error; anything else returns found=false (the "revoked" shape) with no
// error — mirrors *spicedb.Client.ReadAccessTokenGrant's own found=false
// contract.
type stubGrantReader struct {
	grants map[string]spicedb.AccessTokenGrant
	errs   map[string]error
}

func (s stubGrantReader) ReadAccessTokenGrant(_ context.Context, tokenID string) (spicedb.AccessTokenGrant, bool, error) {
	if err, ok := s.errs[tokenID]; ok {
		return spicedb.AccessTokenGrant{}, false, err
	}
	if g, ok := s.grants[tokenID]; ok {
		return g, true, nil
	}
	return spicedb.AccessTokenGrant{}, false, nil
}

type grantReadErr struct{}

func (grantReadErr) Error() string { return "spicedb read boom" }

// tkChecker grants every platform permission to tkAdminID and nothing to
// anyone else — the same binary allow/deny shape handler_test.go's
// stubChecker uses, reused directly from that file (same package).
func tkChecker() stubChecker {
	return stubChecker{allow: map[string]bool{tkAdminID: true}}
}

func tokenCR(name, owner, hash string, expires time.Time, opts ...func(*spiceboxv1alpha1.AccessToken)) *spiceboxv1alpha1.AccessToken {
	at := &spiceboxv1alpha1.AccessToken{
		ObjectMeta: metav1.ObjectMeta{Namespace: tkNS, Name: name},
		Spec: spiceboxv1alpha1.AccessTokenSpec{
			TokenHash:  hash,
			Owner:      owner,
			ClientName: "demo-client",
			ClientID:   "demo-client-id",
			ExpiresAt:  metav1.NewTime(expires),
		},
	}
	for _, o := range opts {
		o(at)
	}
	return at
}

func newTokensAdmind(t *testing.T, checker admind.PlatformChecker, grants admind.AccessTokenGrantReader, objs ...client.Object) (http.Handler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	a, err := admind.New(admind.Config{
		Mem:                  memory.NewLocal(inmem.NewBackend()),
		K8s:                  c,
		Checker:              checker,
		Token:                tkToken,
		Logger:               testr.New(t),
		AccessTokenGrants:    grants,
		AccessTokenNamespace: tkNS,
	})
	require.NoError(t, err)
	return a.Handler(), c
}

type tokenRowWire struct {
	Name         string   `json:"name"`
	Owner        string   `json:"owner"`
	ClientName   string   `json:"clientName"`
	Role         string   `json:"role"`
	ScopeClasses []string `json:"scopeClasses"`
	Unfiltered   bool     `json:"unfiltered"`
	CreatedAt    string   `json:"createdAt"`
	ExpiresAt    string   `json:"expiresAt"`
	LastUsedAt   string   `json:"lastUsedAt"`
	Revoked      bool     `json:"revoked"`
}

type tokensListWire struct {
	Tokens []tokenRowWire `json:"tokens"`
}

// --- (a) list: joins the grant, classifies revoked, never leaks the hash ---

func TestTokensList_JoinsGrantAndNeverLeaksHash(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	full := tokenCR(tkFullName, tkFullOwner, tkFullHash, future, func(at *spiceboxv1alpha1.AccessToken) {
		at.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
		used := metav1.Now()
		at.Status.LastUsedAt = &used
	})
	revoked := tokenCR(tkRevokedName, "bob", tkRevokedHash, future, func(at *spiceboxv1alpha1.AccessToken) {
		at.CreationTimestamp = metav1.NewTime(time.Now())
	})

	grants := stubGrantReader{grants: map[string]spicedb.AccessTokenGrant{
		tkFullName: {
			TokenID:    tkFullName,
			Owner:      identity.CanonicalFromTrusted(tkFullOwner, "test fixture"),
			Role:       accesstoken.RoleFull,
			Unfiltered: true,
			ExpiresAt:  future,
		},
	}}
	h, _ := newTokensAdmind(t, tkChecker(), grants, full, revoked)

	w := do(t, h, http.MethodGet, "/admin/v1/tokens", tkToken, tkAdmin, "")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp tokensListWire
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Tokens, 2)

	byName := map[string]tokenRowWire{}
	for _, r := range resp.Tokens {
		byName[r.Name] = r
	}

	fullRow := byName[tkFullName]
	assert.Equal(t, tkFullOwner, fullRow.Owner)
	assert.Equal(t, "demo-client", fullRow.ClientName)
	assert.Equal(t, accesstoken.RoleFull, fullRow.Role)
	assert.True(t, fullRow.Unfiltered)
	assert.False(t, fullRow.Revoked)
	assert.NotEmpty(t, fullRow.LastUsedAt)

	revokedRow := byName[tkRevokedName]
	assert.True(t, revokedRow.Revoked, "a CR with no SpiceDB tuples must show revoked")
	assert.Equal(t, "unknown", revokedRow.Role)

	// Newest-first: the revoked CR (created last) sorts before the full one.
	require.Len(t, resp.Tokens, 2)
	assert.Equal(t, tkRevokedName, resp.Tokens[0].Name, "newest-created token sorts first")

	// Invariant: the raw body never contains either fixture's tokenHash value.
	assert.NotContains(t, w.Body.String(), tkFullHash)
	assert.NotContains(t, w.Body.String(), tkRevokedHash)
}

// --- (b) a per-item grant-read error degrades that row, not the page -------

func TestTokensList_GrantReadErrorDegradesRowNotPage(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	ok := tokenCR(tkFullName, tkFullOwner, tkFullHash, future)
	broken := tokenCR(tkRevokedName, "bob", tkRevokedHash, future)

	grants := stubGrantReader{
		grants: map[string]spicedb.AccessTokenGrant{
			tkFullName: {TokenID: tkFullName, Owner: identity.CanonicalFromTrusted(tkFullOwner, "test"), Role: accesstoken.RoleRead},
		},
		errs: map[string]error{tkRevokedName: grantReadErr{}},
	}
	h, _ := newTokensAdmind(t, tkChecker(), grants, ok, broken)

	w := do(t, h, http.MethodGet, "/admin/v1/tokens", tkToken, tkAdmin, "")
	require.Equal(t, http.StatusOK, w.Code, "one bad token must not 500 the page; body=%s", w.Body.String())

	var resp tokensListWire
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Tokens, 2)

	byName := map[string]tokenRowWire{}
	for _, r := range resp.Tokens {
		byName[r.Name] = r
	}
	assert.Equal(t, accesstoken.RoleRead, byName[tkFullName].Role)
	assert.Equal(t, "unknown", byName[tkRevokedName].Role, "a grant READ ERROR degrades to unknown, not revoked")
	assert.False(t, byName[tkRevokedName].Revoked, "a read error is not the same claim as 'no tuples found'")
}

func TestTokensList_NonAdminForbidden(t *testing.T) {
	h, _ := newTokensAdmind(t, tkChecker(), stubGrantReader{})
	w := do(t, h, http.MethodGet, "/admin/v1/tokens", tkToken, tkNonAdmin, "")
	assert.Equal(t, http.StatusForbidden, w.Code, "tokens list needs view_tokens")
}

func TestTokensList_MissingServiceTokenUnauthorized(t *testing.T) {
	h, _ := newTokensAdmind(t, tkChecker(), stubGrantReader{})
	w := do(t, h, http.MethodGet, "/admin/v1/tokens", "", tkAdmin, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- (c) revoke --------------------------------------------------------

func TestTokensRevoke_DeletesCR(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	full := tokenCR(tkFullName, tkFullOwner, tkFullHash, future)
	h, c := newTokensAdmind(t, tkChecker(), stubGrantReader{}, full)

	body, err := json.Marshal(map[string]string{"name": tkFullName})
	require.NoError(t, err)
	w := do(t, h, http.MethodPost, "/admin/v1/tokens/revoke", tkToken, tkAdmin, string(body))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		OK bool `json:"ok"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.OK)

	err = c.Get(context.Background(), client.ObjectKey{Namespace: tkNS, Name: tkFullName}, &spiceboxv1alpha1.AccessToken{})
	assert.True(t, apierrors.IsNotFound(err), "the AccessToken CR must be deleted; err=%v", err)
}

func TestTokensRevoke_MissingReturns404(t *testing.T) {
	h, _ := newTokensAdmind(t, tkChecker(), stubGrantReader{})
	body, err := json.Marshal(map[string]string{"name": "at-does-not-exist"})
	require.NoError(t, err)
	w := do(t, h, http.MethodPost, "/admin/v1/tokens/revoke", tkToken, tkAdmin, string(body))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTokensRevoke_NonAdminForbiddenNoDelete(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	full := tokenCR(tkFullName, tkFullOwner, tkFullHash, future)
	h, c := newTokensAdmind(t, tkChecker(), stubGrantReader{}, full)

	body, err := json.Marshal(map[string]string{"name": tkFullName})
	require.NoError(t, err)
	w := do(t, h, http.MethodPost, "/admin/v1/tokens/revoke", tkToken, tkNonAdmin, string(body))
	require.Equal(t, http.StatusForbidden, w.Code)

	getErr := c.Get(context.Background(), client.ObjectKey{Namespace: tkNS, Name: tkFullName}, &spiceboxv1alpha1.AccessToken{})
	assert.NoError(t, getErr, "a denied revoke must not delete the CR")
}

func TestTokensRevoke_MissingServiceTokenUnauthorized(t *testing.T) {
	h, _ := newTokensAdmind(t, tkChecker(), stubGrantReader{})
	body, err := json.Marshal(map[string]string{"name": tkFullName})
	require.NoError(t, err)
	w := do(t, h, http.MethodPost, "/admin/v1/tokens/revoke", "", tkAdmin, string(body))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
