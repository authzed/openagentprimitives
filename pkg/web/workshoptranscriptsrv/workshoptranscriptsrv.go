// Package workshoptranscriptsrv exposes the ONE operator route that lets an
// agent-builder workshop's builder session read back the transcript of a
// session run inside that workshop's own namespace to test what the builder
// built:
//
//	GET /workshop/transcript/{session}   →   {"turns":[...],"auditEntries":[...]}
//
// # Two kinds of test session, two proofs
//
// A builder sees a test run in one of two shapes, and the route authorizes
// each on its own evidence.
//
//   - A DELEGATED CHILD the builder spawned itself carries the `parent`
//     relationship the SubagentRequest controller writes (TouchLineage) in the
//     same reconcile that creates it. read_transcript's `+ parent` arm
//     resolves through exactly that tuple, so what the builder holds over the
//     child IS the delegation, re-checked live.
//   - A ROOT session the PERSON started themselves — the "Try it" link the
//     builder paints, started from the browser under the person's own
//     identity — was delegated by nobody, holds no `parent` relationship, and
//     is never given one. It is admitted instead on the workshop's own
//     recorded ownership: the Workshop's spec.starterCanonical (the
//     AgentSession reconciler's ensureWorkshop stamps it from the builder
//     session's own started-by record) and the session's own started-by
//     record name the same person.
//
// That second arm is deliberately narrow. W is the builder's private scratch
// namespace — created for this workshop, deleted at its teardown — so "a root
// session in W" already means "a session started to try the agent being built
// here", and requiring the two recorded starters to match closes the rest: a
// root session in W that someone ELSE started is not admitted, and neither is
// a delegated child, which keeps the tuple path its own delegation proved.
// Nothing on this path writes a relationship. A parent tuple minted for a
// person's own session would hand the builder a delegator's standing over a
// session the person owns, and would outlive the test that prompted it.
//
// # Ruling A carried over — a narrow re-authorized route, not a broadened bearer
//
// (The same ruling workshopdraftsrv documents for the sibling export route.)
// The workshop sidecar's operator bearer is registered (plan 2) keyed to the
// SYNTHETIC {builderSessionNamespace, WorkshopName(builderSessionName)} pair
// — never to the builder session {B, X} itself, and never to any session in
// its workshop — so LookupInfo alone proves only "this bearer belongs to some
// workshop", nothing about which transcript it may read. Every other piece of
// identity this route needs is derived, never accepted from the caller:
//
//  1. authenticate the bearer (LookupInfo) to learn which Workshop CR it
//     belongs to;
//  2. resolve THAT Workshop CR and read spec.session (the builder session B,
//     in namespace X), status.namespace (W, the workshop namespace) and
//     spec.starterCanonical (the person this workshop belongs to) from it —
//     never from the URL or the request body;
//  3. confirm the {session} the URL names actually lives IN W — its namespace
//     is not a caller-supplied value, it is fixed to the one namespace this
//     bearer's workshop owns;
//  4. authorize on exactly one of the two arms above — the workshop-owner arm
//     for a root session whose recorded starter is the workshop's own, and
//     otherwise a FullyConsistent
//     agentsession:<W>/<session>#read_transcript@agentsession:<X>/<B> check
//     moments before the read it gates;
//  5. only then read that session's turns AND its signed lifecycle log back
//     from the memory backend — both under the one authorization above.
//
// Every step denies closed: a missing/invalid bearer, an unresolvable or
// not-Ready Workshop, a session that does not exist in W, an unreadable
// delegation link, a workshop with no recorded starter, a nil or erroring
// read_transcript checker, and a false permission all refuse the read. A
// bearer can therefore never be asked — and can never be tricked — into
// reading a transcript outside its own workshop's namespace, and never
// without either the delegation the controller itself wrote or the
// workshop's own owner having started the session.
package workshoptranscriptsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// Path is this package's mount prefix on the operator mux — a trailing-slash
// subtree, mirroring secretoutsrv's "/secret-output/". The concrete route
// this package's own mux registers is "GET " + Path + "{session}".
const Path = "/workshop/transcript/"

// ReadTranscriptChecker answers
// agentsession:<childNS>/<childName>#read_transcript@agentsession:<subjectNS>/<subjectName>
// — satisfied by (*pkg/authz/spicedb.Client).CheckReadTranscriptForSession.
// Declared locally (mirrors workshopdraftsrv.WorkshopBuildChecker) for the
// same two reasons that package gives: this package's tests inject a fake
// with no live SpiceDB, and the handler never carries a typed pointer that
// could be assigned nil into an interface field (CLAUDE.md's typed-nil
// rule) — a genuinely unwired dependency is a true nil interface, caught by
// the nil check in ServeHTTP, not a panic.
type ReadTranscriptChecker interface {
	CheckReadTranscriptForSession(ctx context.Context, childNS, childName, subjectNS, subjectName string) (bool, error)
}

// transcriptResponse is the 200 response body: the read session's turns, in
// (index, role) order exactly as turn.ReadAll returns them, and its signed
// lifecycle log in fold order.
//
// The two lists answer different questions and a reader needs both. A session
// that failed BEFORE it called anything has an empty Turns — and a reader
// handed only that concludes nothing ever happened, which is what a builder
// concluded about a run whose runner refused to start. AuditEntries is where
// that run's own account of itself lives.
type transcriptResponse struct {
	Turns        []memory.Turn `json:"turns"`
	AuditEntries []auditEntry  `json:"auditEntries"`
}

// auditEntry is one entry of the session's signed lifecycle log, flattened for
// a reader: when it was recorded, which transition it was, and the transition's
// own payload (a failure's reason and message, a decision's id, …) exactly as
// the signed entry carries it. Nothing is RE-WORDED here — the log is evidence,
// and a summary of evidence is not the evidence.
//
// It is not, however, the stored bytes verbatim: Details comes from
// auditEntries' re-marshal of the DECODED Go event (lifecyclekind.ReadOrdered
// then json.Marshal), not a pass-through of what the signed entry stored on
// disk. An entry whose type this build cannot name — an older or newer event
// kind — is skipped rather than rendered, and is logged when that happens.
// What a reader gets is this build's own understanding of the entry, not a
// byte-for-byte copy of it.
type auditEntry struct {
	At      time.Time       `json:"at"`
	Type    string          `json:"type"`
	Details json.RawMessage `json:"details,omitempty"`
}

// NewHandler returns the GET /workshop/transcript/{session} handler.
//
//   - c resolves the caller's own Workshop CR (to derive B/X, W and the
//     workshop's starter), confirms the named AgentSession actually lives in
//     W, and reads that session's owning SubagentRequest to tell a delegated
//     child from a root test session. Reads only.
//   - mem reads the session's turns back once authorized — an in-process,
//     trusted operator caller, so it mints its own system approval for the
//     memory data plane's read door the same way workshopdraftsrv mints one
//     for the write door.
//   - reg authenticates the bearer.
//   - checker answers read_transcript for the builder session; nil is
//     handled at request time (denied, not panicked) per the typed-nil rule
//     on ReadTranscriptChecker.
func NewHandler(c client.Client, mem memory.Memory, reg *tokens.Registry, checker ReadTranscriptChecker) http.Handler {
	h := &handler{c: c, mem: mem, reg: reg, checker: checker}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Path+"{session}", h.serveHTTP)
	return mux
}

type handler struct {
	c       client.Client
	mem     memory.Memory
	reg     *tokens.Registry
	checker ReadTranscriptChecker
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-transcript"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, prefix)

	// The bearer resolves to the SYNTHETIC workshop key
	// {builderSessionNamespace, WorkshopName(builderSessionName)} the
	// AgentSession reconciler registered it under (plan 2) — this proves the
	// bearer belongs to SOME workshop, nothing about which session it may read.
	info, ok := h.reg.LookupInfo(token)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="workshop-transcript"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	var ws spiceboxv1alpha1.Workshop
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: info.Session.Namespace, Name: info.Session.Name}, &ws); err != nil {
		http.Error(w, fmt.Sprintf("no workshop resolves for this bearer: %v", err), http.StatusForbidden)
		return
	}
	if ws.Status.Namespace == "" || ws.Status.Phase != spiceboxv1alpha1.WorkshopPhaseReady {
		http.Error(w, fmt.Sprintf("workshop %s/%s is not Ready (phase %q)", ws.Namespace, ws.Name, ws.Status.Phase), http.StatusForbidden)
		return
	}
	// B/X and W come ENTIRELY from the resolved CR — never the URL, other
	// than the session's bare NAME, which is the one thing the route takes.
	parentNS, parentName, workshopNS := ws.Spec.Session.Namespace, ws.Spec.Session.Name, ws.Status.Namespace

	sessionName := r.PathValue("session")
	if sessionName == "" {
		http.Error(w, "missing session name", http.StatusBadRequest)
		return
	}

	// The session MUST live in the bearer's OWN workshop namespace W — never a
	// namespace the request supplies. A session named the same as one in a
	// DIFFERENT workshop's namespace is simply not found here and refuses
	// before authorization is even asked.
	var sess spiceboxv1alpha1.AgentSession
	if err := h.c.Get(ctx, types.NamespacedName{Namespace: workshopNS, Name: sessionName}, &sess); err != nil {
		http.Error(w, fmt.Sprintf("session %s/%s not found in this workshop: %v", workshopNS, sessionName, err), http.StatusForbidden)
		return
	}

	// Fail-closed on a nil checker: an unwired dependency must never read as
	// "no permission to check, so allow" (CLAUDE.md's typed-nil rule; mirrors
	// workshopdraftsrv's h.build == nil guard). It guards BOTH arms below,
	// including the owner arm that would not have consulted it: a route whose
	// authorization backend never got wired is a deployment fault, not a mode
	// of operation, and it reads nothing.
	if h.checker == nil {
		http.Error(w, "no read_transcript authorization backend is wired; refusing to read", http.StatusServiceUnavailable)
		return
	}

	// Arm one: the workshop's owner reading their own root test session.
	//
	// OwningSubagentRequest keeps "delegated by nobody" and "names a parent
	// whose request cannot be read" apart, and only the first is a root
	// session. An error is therefore a refusal, never a shrug — reading a
	// broken delegation link as "not delegated" would re-score a delegated
	// child as a person's own test and skip the very tuple that gates it.
	owningReq, err := spiceboxv1alpha1.OwningSubagentRequest(ctx, h.c, &sess)
	if err != nil {
		log.FromContext(ctx).Info("workshop transcript: refusing the read; the named session's delegation link could not be read, so whether it is the workshop owner's own root test session is unknowable",
			"workshop", ws.Namespace+"/"+ws.Name, "session", workshopNS+"/"+sessionName, "err", err.Error())
		http.Error(w, fmt.Sprintf("cannot determine whether %s/%s was delegated: %v", workshopNS, sessionName, err), http.StatusForbidden)
		return
	}
	// spec.starterCanonical is +optional and StartedByCanonical returns "" for
	// a session carrying no started-by annotation, so an empty starter must
	// never compare equal to an empty/absent canonical — that would make a
	// session NOBODY is attributed with readable as this person's own test.
	// Fail closed, the same comparison pkg/controllers/workshop/testwatch.go's
	// qualifyingTestSession and the workshop sidecar's test_sessions tool
	// apply to pick out the person's own test runs.
	ownerStartedRoot := owningReq == nil &&
		ws.Spec.StarterCanonical != "" &&
		spiceboxv1alpha1.StartedByCanonical(&sess).String() == ws.Spec.StarterCanonical

	if ownerStartedRoot {
		log.FromContext(ctx).Info("workshop transcript: admitting a root test session started by this workshop's own owner",
			"workshop", ws.Namespace+"/"+ws.Name, "session", workshopNS+"/"+sessionName)
	} else {
		// Arm two: everything else in W, on the relationship its delegation
		// wrote. FullyConsistent per CheckReadTranscriptForSession's own
		// contract: this is a privilege gate asked moments before the read it
		// protects. An error is fail-closed too — an unconfirmable permission
		// is a no, not a maybe.
		allowed, err := h.checker.CheckReadTranscriptForSession(ctx, workshopNS, sessionName, parentNS, parentName)
		if err != nil {
			http.Error(w, fmt.Sprintf("checking read_transcript for %s/%s: %v", workshopNS, sessionName, err), http.StatusForbidden)
			return
		}
		if !allowed {
			http.Error(w, fmt.Sprintf("session %s/%s does not hold read_transcript for %s/%s", parentNS, parentName, workshopNS, sessionName), http.StatusForbidden)
			return
		}
	}

	// This route is an in-process, trusted operator caller — not a session
	// bearer — so it authorizes its own memory read the way every other
	// in-process reader does: a system approval, minted here rather than
	// carried on ctx from anywhere else (memory.WithSystemApproval; see
	// workshopdraftsrv.ServeHTTP for the write-side precedent). The arm above
	// already authorized this specific read; this only satisfies the memory
	// data plane's OWN capability door (pkg/memory/approval.go).
	approvedCtx := memory.WithSystemApproval(ctx, "operator:workshop-transcript-route")
	scope := memory.Scope{Kind: "session", ID: workshopNS + "/" + sessionName}
	turns, err := turn.ReadAll(approvedCtx, h.mem, scope)
	if err != nil {
		http.Error(w, "read session transcript: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Same scope, same authorization, same approval: the signed lifecycle log is
	// this session's own account of itself, and it is the ONLY account when the
	// session failed before it said or did anything.
	entries, err := h.auditEntries(approvedCtx, scope)
	if err != nil {
		http.Error(w, "read session log: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(transcriptResponse{Turns: turns, AuditEntries: entries}); err != nil {
		// The status line and headers are already committed, so this can only be
		// logged — but a caller that got a truncated body and no explanation is
		// exactly the silent failure this route exists to end.
		log.FromContext(ctx).Info("workshop transcript: writing the response body failed",
			"session", workshopNS+"/"+sessionName, "err", err.Error())
	}
}

// auditEntries reads the session's signed lifecycle log in fold order and
// flattens each transition to its recorded time, wire-type name and payload.
//
// An entry whose type this build cannot name is SKIPPED and logged rather than
// failing the read: the Kind is append-only, so an entry written by a newer
// publisher can never be removed, and a hard error here would make every later
// read of the session fail permanently over one unrecognized transition.
func (h *handler) auditEntries(ctx context.Context, scope memory.Scope) ([]auditEntry, error) {
	ordered, err := lifecyclekind.ReadOrdered(ctx, h.mem, scope)
	if err != nil {
		return nil, err
	}
	out := make([]auditEntry, 0, len(ordered))
	for _, oe := range ordered {
		typ, err := lifecyclekind.TypeName(oe.Event)
		if err != nil {
			log.FromContext(ctx).Info("workshop transcript: skipping a lifecycle entry this build cannot name",
				"session", scope.ID, "entry", oe.EntryID, "err", err.Error())
			continue
		}
		details, err := json.Marshal(oe.Event)
		if err != nil {
			log.FromContext(ctx).Info("workshop transcript: skipping a lifecycle entry whose payload will not encode",
				"session", scope.ID, "entry", oe.EntryID, "type", typ, "err", err.Error())
			continue
		}
		entry := auditEntry{At: oe.CreatedAt, Type: typ}
		// A payload-free transition marshals to "{}"; omit it rather than
		// shipping an empty object on every entry that carries no detail.
		if string(details) != "{}" {
			entry.Details = details
		}
		out = append(out, entry)
	}
	return out, nil
}
