package httpsrv

import (
	"context"
	"encoding/json"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
)

// The resolve route hands a caller the CONTENT behind a tag — but only content
// it is entitled to receive. It is the third of the pt-tag routes and the only
// one that returns bytes: _pttag_mint writes provenance, _pttag_verify confirms
// ids, and this resolves content for the two platform paths that legitimately
// need a datum back — derive_tag's validator (it reads the source tags it
// derives from) and a subagent's bound data slot (a child needs its parent's
// bound datum). Both read pt_tag_content, which is component-read
// (SessionReadable false), so neither can read it over the memory API itself;
// the operator holds the credential.
//
// Entitlement is per-tag, gated on pt_tag:<id>#access (session + ancestor +
// granted_to) for the CALLER's own session — the credential's identity, not the
// URL scope, because a child reads its PARENT's scope but is entitled via its
// own granted_to binding. A tag the caller lacks access to is omitted, never
// returned: this route must not become a read-back channel for arbitrary
// pt_tag_content, which is the whole reason that kind is component-read.

// PtTagAccessChecker answers whether a session may receive a tag's content —
// pt_tag:<tagID>#access for agentsession:<session>. Implemented by the operator,
// which holds the SpiceDB reader; nil disables the route (405).
type PtTagAccessChecker interface {
	HasTagAccess(ctx context.Context, tagID string, session memory.NamespacedName) (bool, error)
}

// WithPtTagResolver enables POST /memory/_pttag_resolve/{ns}/{name}.
func WithPtTagResolver(c PtTagAccessChecker) HandlerOption {
	return func(h *handler) { h.ptTagAccess = c }
}

func (h *handler) handlePtTagResolve(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if h.ptTagAccess == nil {
		http.Error(w, "pt-tag content resolution is not enabled on this server", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	// The caller's OWN session (the credential's identity), not the URL scope:
	// entitlement is the caller's granted_to/lineage, and a child reads its
	// parent's scope. Present-not-empty per WithTokenSession's contract; a
	// resolve with no session credential has no entitlement to check and is
	// refused.
	caller, ok := memory.TokenSessionFrom(r.Context())
	if !ok {
		http.Error(w, "pt-tag resolve requires a per-session credential", http.StatusForbidden)
		return
	}

	var req memory.PtTagResolveRequest
	limitJSONBody(w, r)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "decode PtTagResolveRequest: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Component read of the content store: drop the token-session mark (the
	// kind is component-read) and clear the capability door, exactly as the
	// mint's content write and the verify route do. The scope the router already
	// authorized against this token bounds WHERE the bytes may come from; the
	// per-tag access check below bounds WHICH the caller may receive.
	cctx := memory.WithSystemApproval(memory.WithoutTokenSession(r.Context()), "pt_resolve")
	recs, err := pttagcontent.List(cctx, h.mem, scope)
	if err != nil {
		log.FromContext(r.Context()).Info("memory: pt-tag resolve could not read content store",
			"scope", scope.ID, "err", err.Error())
		http.Error(w, "reading content store: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// ResolveEntitled gates each present tag on the caller's pt_tag#access and is
	// shared with the in-process e2e harness so both filter identically.
	contents, err := pttagcontent.ResolveEntitled(recs, req.TagIDs, func(tagID string) (bool, error) {
		return h.ptTagAccess.HasTagAccess(r.Context(), tagID, caller)
	})
	if err != nil {
		log.FromContext(r.Context()).Info("memory: pt-tag resolve access check errored",
			"session", caller.Namespace+"/"+caller.Name, "err", err.Error())
		http.Error(w, "resolve access check: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := memory.PtTagResolveResponse{Contents: contents}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.FromContext(r.Context()).Info("memory: pt-tag resolve response write failed",
			"scope", scope.ID, "err", err.Error())
	}
}
