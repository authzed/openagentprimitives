package httpsrv

import (
	"encoding/json"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
)

// The verify route is the READ-side mirror of the mint route, and it exists for
// the same reason: pt_tag_content is component-read as well as component-written
// (its SessionReadable is false), so the bytes behind a tag live somewhere a
// session bearer cannot reach over this API.
//
// The per-datum egress gate needs to confirm that the bytes an outbound payload
// carries under a claimed tag id ARE the bytes the platform stored for that id —
// otherwise a caller could pair a witnessed wide id with fabricated content and
// disclose to the wide audience. That comparison has to happen where the stored
// bytes are, and by a party the caller cannot subvert: if the runner did the
// compare, a compromised runner would simply declare a match. So the runner
// parses the payload's regions (a parse, not a privilege) and POSTs them here;
// the operator, holding the component credential, reads pt_tag_content and
// returns only WHICH ids bound — never the bytes, so this is not a read-back
// channel for content a session may not have.
//
// Unlike the mint route there is no injected collaborator to enable it: the
// operator already holds the memory facade this reads, so the route is always
// available. It reads component-side by dropping the token-session mark exactly
// as the minter does at its authorship hand-off — the scope it may read was
// already authorized against the caller's token by the router (Authorizes),
// before this handler runs.

func (h *handler) handlePtTagVerify(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var req memory.PtTagVerifyRequest
	limitJSONBody(w, r)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "decode PtTagVerifyRequest: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Component read: drop the token-session mark so the per-kind read door —
	// which refuses a token-originated read of a component-only kind by design —
	// lets the operator read pt_tag_content it authored. WithSystemApproval
	// clears the capability door for the same in-process platform read the
	// mint's content write already relies on. The scope is the URL scope the
	// router already authorized against this token, so this reads only the
	// caller's own session, never another's.
	ctx := memory.WithSystemApproval(memory.WithoutTokenSession(r.Context()), "pt_egress_verify")
	recs, err := pttagcontent.List(ctx, h.mem, scope)
	if err != nil {
		// Fail closed: the caller (the leak gate) treats a failed verify as
		// "cannot establish per-datum coverage" and drops to the coarse floor,
		// which over-blocks. Logged so an operator can tell a real store fault
		// from an empty scope.
		log.FromContext(r.Context()).Info("memory: pt-tag verify could not read content store",
			"scope", scope.ID, "err", err.Error())
		http.Error(w, "reading content store: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Bind is shared with the in-process e2e harness so both compare identically.
	resp := pttagcontent.Bind(recs, req.Regions)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.FromContext(r.Context()).Info("memory: pt-tag verify response write failed",
			"scope", scope.ID, "err", err.Error())
	}
}
