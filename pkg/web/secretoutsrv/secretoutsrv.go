// Package secretoutsrv exposes an authenticated operator HTTP endpoint that
// a runner POSTs a captured secret-output value to; the operator (NOT the
// runner) writes it into the per-session secret-output Secret.
//
//	POST /secret-output/{ns}/{name}  body SecretOutputRequest → 204
//
// This is the operator side of the operator-mediated secret-write design
// (least-privilege): the runner has NO Secret-write RBAC. {ns}/{name} identify
// the AgentSession.
//
// Auth: Authorization: Bearer <token>. Only a per-session token (per the
// supplied tokens.Registry) whose PRIMARY session is the URL's {ns}/{name} may
// write — not merely one whose authorized-session set contains it, since that
// set also holds read-only extra scopes. This is session-scoped by design, so
// the system channelsd/authzd tokens are NOT accepted here (a missing/foreign
// token is a 401/403). The endpoint receives secret VALUES and writes Secrets,
// so it is security-sensitive: values are never logged.
package secretoutsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// maxSecretOutputBody bounds the POST body before it is decoded.
//
// Every sibling route in this codebase bounds its own; this one did not, so an
// unbounded json.Decode from ANY per-session token holder could pin the
// operator's memory for as long as the caller kept writing. A secret-output
// body is a logical name plus one value — generous here for the largest
// credential anyone stores, and far below a size that matters.
const maxSecretOutputBody = 1 << 20 // 1 MiB

// ErrSecretOutputConflict is returned by writeSecretOutput when the
// requested key already exists in the per-session Secret. ServeHTTP maps it
// to 409 Conflict; every other writeSecretOutput error maps to 500.
var ErrSecretOutputConflict = errors.New("secret-output: name already satisfied for this session (write-once)")

// SecretOutputRequest is the POST body. The routing and storage keys come from
// the URL ({ns}/{name}) and Name — the body never carries session identity.
type SecretOutputRequest struct {
	// Name is the secret-output logical name; it becomes the Secret's Data key.
	Name string `json:"name"`
	// Value is the secret value written under Data[Name]. Never logged.
	Value string `json:"value"`
	// Handle is the opaque so-... handle (correlation only).
	Handle string `json:"handle"`
}

// SecretOutputSecretName is the deterministic per-session secret-output Secret
// name. Runner and operator agree on this derivation so the runner can record
// SecretName in status, and the suffix is the one
// ToolCall.ValidateCredentialSourceOwnership binds to the owning session.
func SecretOutputSecretName(sessionName string) string {
	return sessionName + spiceboxv1alpha1.SecretOutputSecretSuffix
}

// NewHandler returns an http.Handler mounting POST /secret-output/{ns}/{name}.
// c writes the per-session Secret (owner-ref'd to the AgentSession it Gets);
// reg authenticates per-session bearer tokens.
func NewHandler(c client.Client, reg *tokens.Registry) http.Handler {
	h := &handler{c: c, reg: reg}
	mux := http.NewServeMux()
	mux.Handle("/secret-output/", h)
	return mux
}

type handler struct {
	c   client.Client
	reg *tokens.Registry
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="secret-output"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, prefix)

	// Path: /secret-output/{ns}/{name}. {ns}/{name} identify the AgentSession
	// and are the sole source of the routing/storage keys — the body is never
	// trusted for session identity.
	rest := strings.TrimPrefix(r.URL.Path, "/secret-output/")
	segs := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		http.Error(w, "not a secret-output path: expected /secret-output/{ns}/{name}", http.StatusNotFound)
		return
	}
	ns, name := segs[0], segs[1]
	urlSess := memory.NamespacedName{Namespace: ns, Name: name}

	// Auth: per-session tokens ONLY. System channelsd/authzd tokens are
	// deliberately not accepted — these writes are session-scoped, and a system
	// token would not appear in the per-session registry, so LookupInfo misses
	// (401). A registered token that is not authorized for this {ns}/{name} is
	// a 403.
	if _, ok := h.reg.LookupInfo(token); !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="secret-output"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// AuthorizesMutation, never Authorizes: this endpoint serves POST alone, so
	// there is no read for the READ predicate to answer. A per-session token
	// also carries EXTRA scopes — the per-bundle SpiceboxSessions whose ToolCall
	// artifacts the runner fetches — which tokens.Set makes read-only by
	// contract. Authorizes is true for all of them, so it would let a runner
	// write a write-once secret-output onto a foreign session it may only look
	// at. AuthorizesMutation admits the token's PRIMARY session and nothing else.
	if !h.reg.AuthorizesMutation(token, urlSess) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Bounded before decoding, which every sibling route already does and this
	// one did not: a secret-output body is a name plus one value, and an
	// unbounded Decode from any per-session token holder pins the operator's
	// memory for as long as the caller keeps writing. The cap is generous for
	// the largest credential anyone stores and far below anything that matters.
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretOutputBody)
	var req SecretOutputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Never echo the body; the decode error text is structural only.
		http.Error(w, "decode SecretOutputRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if err := h.writeSecretOutput(r.Context(), ns, name, req.Name, []byte(req.Value)); err != nil {
		if errors.Is(err, ErrSecretOutputConflict) {
			// Write-once conflict: the key exists with a DIFFERENT value (a
			// byte-identical re-put is idempotent and returns 204 without
			// reaching here). Never echo/log the value; the key name is not
			// sensitive. Defense in depth — the runner's fast-fail pre-check
			// keeps legitimate retries from arriving here at all.
			http.Error(w, fmt.Sprintf("secret-output %q already satisfied for this session (write-once)", req.Name), http.StatusConflict)
			return
		}
		// Surface a structural message + log without the value. The handle is
		// safe to include (opaque, not the secret).
		http.Error(w, fmt.Sprintf("write secret-output %q: %v", req.Name, err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSecretOutput merges value under Data[key] into the per-session
// secret-output Secret, owner-ref'd to the AgentSession. Idempotent: Create,
// and on AlreadyExists Get + Update merging only this key so sibling
// secret-output keys are preserved (NOT clobbered).
func (h *handler) writeSecretOutput(ctx context.Context, ns, sessName, key string, value []byte) error {
	// Resolve the AgentSession for the owner-ref UID. A missing session is a
	// caller bug (token authorized for a session the operator can't see) — fail
	// loud rather than writing an unowned Secret.
	var sess spiceboxv1alpha1.AgentSession
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: sessName}, &sess); err != nil {
		return fmt.Errorf("get AgentSession %s/%s: %w", ns, sessName, err)
	}

	secName := SecretOutputSecretName(sessName)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secName,
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:               "AgentSession",
				Name:               sess.Name,
				UID:                sess.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{key: value},
	}
	// Stamp the adoption label so the operator's guarded SecretReader (and the
	// label-filtered cache) accept this operator-minted secret-output Secret.
	adoptguard.WithAdoptedLabel(sec)
	if err := h.c.Create(ctx, sec); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create secret %q: %w", secName, err)
		}
		// Merge: preserve sibling keys, overwrite only this key.
		var existing corev1.Secret
		if getErr := h.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secName}, &existing); getErr != nil {
			return fmt.Errorf("get existing secret %q: %w", secName, getErr)
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		if prev, ok := existing.Data[key]; ok {
			// Write-once FOR VALUE: a satisfied secret-output name is immutable
			// for the session's lifetime — load-bearing for cluster pinning, so
			// a second fetch-kubeconfig cannot swap the sidecar's mounted
			// credential out from under it.
			//
			// A byte-identical re-put is IDEMPOTENT (nil → 204), recovering the
			// case where the Secret landed but the response was lost and the
			// client retried. The comparison is bytes-equal ONLY and neither
			// value is ever logged. A DIFFERENT value is the genuine write-once
			// conflict (409) — a stale or raced runner retry — and must not
			// silently overwrite.
			if bytes.Equal(prev, value) {
				return nil
			}
			return fmt.Errorf("%w: %q", ErrSecretOutputConflict, key)
		}
		existing.Data[key] = value
		adoptguard.WithAdoptedLabel(&existing) // ensure the label is present on a pre-existing secret
		if updErr := h.c.Update(ctx, &existing); updErr != nil {
			return fmt.Errorf("update secret %q: %w", secName, updErr)
		}
	}
	return nil
}
