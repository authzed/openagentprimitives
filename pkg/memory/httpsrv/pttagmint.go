package httpsrv

import (
	"context"
	"encoding/json"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// The mint route exists because of an asymmetry the write door creates.
//
// pt_tag is ComponentWritten: a session credential may not author one, because
// a tag's direct_reader set GRANTS disclosure — a wider set permits more — so a
// session able to write its own tags could name an audience its source never
// authorized and then disclose to it legitimately, since the tag would be
// checked and would say yes.
//
// But the moment a datum enters context IS on the runner's tool-call path, and
// only the runner knows which call carried which resource. So the runner ASKS
// and a component ANSWERS: the request names the RESOURCES touched, never the
// readers, and the minter performs the LookupSubjects itself. That split is the
// whole security property of this route. A request that could carry a reader
// set would hand the pen straight back.
//
// The wire types live in pkg/memory (pttagwire.go) rather than here, so the
// client that calls this route need not import the server that serves it.

// PtTagMinter derives a datum's audience and records the tag. Implemented by
// the operator, which holds both the SpiceDB reader and the component memory
// credential; nil disables the route (405).
//
// The interface deliberately does not accept an audience. Everything the
// caller supplies is a claim about what it TOUCHED, which the minter can and
// does re-derive; nothing it supplies is a claim about who may SEE it.
type PtTagMinter interface {
	MintPtTag(ctx context.Context, scope memory.Scope, req memory.PtTagMintRequest) (string, error)
}

// WithPtTagMinter enables POST /memory/_pttag_mint/{ns}/{name}.
func WithPtTagMinter(m PtTagMinter) HandlerOption {
	return func(h *handler) { h.ptTagMinter = m }
}

func (h *handler) handlePtTagMint(w http.ResponseWriter, r *http.Request, scope memory.Scope) {
	if h.ptTagMinter == nil {
		// Not wired is not the same as refused. A 405 says the platform does
		// not offer per-datum provenance here, which is the honest answer for
		// a deployment that never enabled it — and it is distinguishable from
		// the 403 a real authorization refusal produces.
		http.Error(w, "pt-tag minting is not enabled on this server", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var req memory.PtTagMintRequest
	limitJSONBody(w, r)
	dec := json.NewDecoder(r.Body)
	// A typo'd key must fail rather than be dropped. Silently ignoring an
	// unknown field is how a caller believes it marked a datum untrusted while
	// the minter recorded it clean.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "decode PtTagMintRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Resources) == 0 && len(req.DerivedFrom) == 0 {
		http.Error(w, "a mint needs either resources (leaf) or derivedFrom (derived)", http.StatusBadRequest)
		return
	}
	if len(req.Resources) > 0 && len(req.DerivedFrom) > 0 {
		// Leaf XOR derived, refused at the edge as well as in the composer.
		// A tag that were both would be read through both arms of `reader`,
		// whose + is a union, and would resolve wider than its sources allow.
		http.Error(w, "a mint is leaf or derived, never both", http.StatusBadRequest)
		return
	}

	id, err := h.ptTagMinter.MintPtTag(r.Context(), scope, req)
	if err != nil {
		status := sentinelStatus(err)
		log.FromContext(r.Context()).Info("memory: pt-tag mint failed",
			"scope", scope.ID, "toolUseID", req.ToolUseID, "status", status, "err", err.Error())
		httpError(w, err, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(memory.PtTagMintResponse{TagID: id}); err != nil {
		// The tag IS minted at this point; only the reply was lost. Logged
		// rather than dropped so a caller's retry — which would mint a second
		// tag for the same datum — has something to be correlated against.
		log.FromContext(r.Context()).Info("memory: pt-tag minted but response write failed",
			"scope", scope.ID, "tagID", id, "err", err.Error())
	}
}
