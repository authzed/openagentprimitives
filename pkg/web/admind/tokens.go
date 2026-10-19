package admind

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// maxTokenRevokeBody caps the revoke POST body — it carries nothing but a
// CR name, so this is generous rather than load-bearing.
const maxTokenRevokeBody = 4096

// roleUnknown is the role shown for a row admind could not resolve a SpiceDB
// grant for (a read error, or no AccessTokenGrants reader configured) — NOT
// the same as revoked: an unknown role means "we couldn't tell", a revoked
// row means ReadAccessTokenGrant positively found no tuples.
const roleUnknown = "unknown"

// AccessTokenGrantReader reads an AccessToken's SpiceDB authorization grant
// (role, scope classes, unfiltered) for display on the admin tokens page —
// *spicedb.Client.ReadAccessTokenGrant (Task 3) satisfies this.
//
// OPTIONAL and NOT in admind.New's required-deps check: nil degrades every
// row's role to "unknown" rather than failing admind construction — the same
// degrade-not-fail treatment SubjectIdentityReader/SourceScopeReader get. The
// CR list itself (name/owner/client/expiry/last-used) renders either way,
// because those fields come from the CR, never from SpiceDB.
type AccessTokenGrantReader interface {
	ReadAccessTokenGrant(ctx context.Context, tokenID string) (spicedb.AccessTokenGrant, bool, error)
}

// accessTokenRow is one AccessToken projected for the admin Tokens table.
// Deliberately omits spec.TokenHash: the hash is a credential-adjacent value
// that must never reach a response, a log line, or this struct.
type accessTokenRow struct {
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	ClientName string `json:"clientName,omitempty"`
	// Role is the SpiceDB role ladder this token climbed ("read"/"interact"/
	// "full"), or roleUnknown when the grant could not be read.
	Role         string   `json:"role"`
	ScopeClasses []string `json:"scopeClasses,omitempty"`
	Unfiltered   bool     `json:"unfiltered"`
	CreatedAt    string   `json:"createdAt"`
	ExpiresAt    string   `json:"expiresAt"`
	LastUsedAt   string   `json:"lastUsedAt,omitempty"`
	// Revoked is true when the CR still exists but ReadAccessTokenGrant found
	// no SpiceDB tuples for it — a token whose authorization was pulled out
	// from under an otherwise-present CR.
	Revoked bool `json:"revoked"`
}

// tokensResponse is the GET /admin/v1/tokens payload.
type tokensResponse struct {
	Tokens []accessTokenRow `json:"tokens"`
}

// handleTokensList lists every AccessToken in cfg.AccessTokenNamespace,
// joining each one's SpiceDB grant for its role/scope. A per-item grant-read
// error (or no AccessTokenGrants configured at all) degrades that ROW to
// role "unknown" — logged — rather than 500ing the whole page: one bad token
// must not blank the console for every admin.
func (a *Admind) handleTokensList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.cfg.AccessTokenNamespace == "" {
		// Fail closed: client.InNamespace("") lists CLUSTER-WIDE, which is never
		// the intended behavior for an unconfigured namespace.
		a.cfg.Logger.Info("admind: tokens list refused; AccessTokenNamespace not configured")
		writeJSONError(w, http.StatusInternalServerError, "access token namespace not configured")
		return
	}

	var list spiceboxv1alpha1.AccessTokenList
	if err := a.cfg.K8s.List(ctx, &list, client.InNamespace(a.cfg.AccessTokenNamespace)); err != nil {
		a.cfg.Logger.Info("admind: tokens list failed", "namespace", a.cfg.AccessTokenNamespace, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "list access tokens failed: "+err.Error())
		return
	}

	if a.cfg.AccessTokenGrants == nil {
		a.cfg.Logger.Info("admind: tokens list: no AccessTokenGrants reader configured; every row's role is unavailable")
	}

	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].CreationTimestamp.Time, items[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return items[i].Name < items[j].Name
	})

	rows := make([]accessTokenRow, 0, len(items))
	for i := range items {
		rows = append(rows, a.projectAccessToken(ctx, &items[i]))
	}
	writeJSON(w, http.StatusOK, tokensResponse{Tokens: rows})
}

// projectAccessToken builds one table row from an AccessToken CR plus (when
// available) its SpiceDB grant. Everything that can come from the CR alone —
// name, owner, client, created/expires, last-used — is filled in first and
// unconditionally, so a grant-read failure never blanks those fields, only
// Role/ScopeClasses/Unfiltered/Revoked.
func (a *Admind) projectAccessToken(ctx context.Context, at *spiceboxv1alpha1.AccessToken) accessTokenRow {
	row := accessTokenRow{
		Name:       at.Name,
		Owner:      at.Spec.Owner,
		ClientName: at.Spec.ClientName,
		Role:       roleUnknown,
		CreatedAt:  at.CreationTimestamp.UTC().Format(time.RFC3339),
		ExpiresAt:  at.Spec.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if at.Status.LastUsedAt != nil {
		row.LastUsedAt = at.Status.LastUsedAt.Time.UTC().Format(time.RFC3339)
	}

	if a.cfg.AccessTokenGrants == nil {
		return row
	}

	grant, found, err := a.cfg.AccessTokenGrants.ReadAccessTokenGrant(ctx, at.Name)
	if err != nil {
		a.cfg.Logger.Info("admind: read access token grant failed", "token", at.Name, "err", err.Error())
		return row
	}
	if !found {
		row.Revoked = true
		return row
	}
	row.Role = grant.Role
	row.ScopeClasses = grant.ScopeClasses
	row.Unfiltered = grant.Unfiltered
	return row
}

// accessTokenRevokeRequest is the POST /admin/v1/tokens/revoke body.
type accessTokenRevokeRequest struct {
	Name string `json:"name"`
}

// accessTokenRevokeResponse is the 200 body.
type accessTokenRevokeResponse struct {
	OK bool `json:"ok"`
}

// handleTokensRevoke deletes the named AccessToken CR in
// cfg.AccessTokenNamespace — the CR's finalizer removes its SpiceDB tuples,
// so admind itself never touches SpiceDB for a revoke. 404 when the CR is
// already absent.
func (a *Admind) handleTokensRevoke(w http.ResponseWriter, r *http.Request) {
	if a.cfg.AccessTokenNamespace == "" {
		a.cfg.Logger.Info("admind: token revoke refused; AccessTokenNamespace not configured")
		writeJSONError(w, http.StatusInternalServerError, "access token namespace not configured")
		return
	}

	var body accessTokenRevokeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTokenRevokeBody)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse JSON body: "+err.Error())
		return
	}
	if body.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	obj := &spiceboxv1alpha1.AccessToken{}
	obj.Namespace, obj.Name = a.cfg.AccessTokenNamespace, body.Name
	subject := r.Header.Get("X-Admin-Subject")
	if err := a.cfg.K8s.Delete(r.Context(), obj); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, "access token not found")
			return
		}
		a.cfg.Logger.Info("admind: token revoke failed", "token", body.Name, "subject", subject, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "revoke failed: "+err.Error())
		return
	}
	a.cfg.Logger.Info("admind: access token revoked by admin", "token", body.Name, "subject", subject)
	writeJSON(w, http.StatusOK, accessTokenRevokeResponse{OK: true})
}
