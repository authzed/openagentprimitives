// Package memoryref is the "memory" uibindings.Resolver: a read-only
// memory.Query scoped to the viewer's own session. It never accepts a scope,
// namespace or session from the binding's Args — the Query is always built
// from req.Namespace/req.Session, populated from the URL path the viewer's
// CheckInteract already gated.
//
// The session-scoped webd memory token is the OTHER layer: the token bounds
// which session's data a query can even reach, while this resolver decides
// which session gets asked. Neither substitutes for the other.
package memoryref

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/registry"
)

// source is this resolver's uibindings.Request.Source / Resolver.Source()
// value, and the string a component prop's Binding declares to route here.
const source = "memory"

// tooLargeMessage is the browser-facing copy for a result over the UI
// ingress ceiling — plain human copy with no memory-kind vocabulary or byte
// counts, matching the posture toolguard's own ui_ingress deny message takes
// for the tool-call binding path (pkg/authz/toolguard/events.go's byteDenyMessage).
const tooLargeMessage = "This view's data is too large to display. Narrow the time span, add a filter, or ask for fewer rows."

// Resolver is the "memory" uibindings.Resolver.
type Resolver struct{}

// New returns the "memory" resolver.
func New() uibindings.Resolver { return Resolver{} }

func init() { registry.Register(New()) }

// Source implements uibindings.Resolver.
func (Resolver) Source() string { return source }

// queryArgs is the CLOSED set of query fields a memory binding may express.
// Decoded with DisallowUnknownFields so an invented field is a loud
// rejection rather than a silently-ignored filter — the same posture
// validateProps takes for component props, and for the same reason: the args
// may be agent-authored.
//
// There is deliberately NO scope, namespace, or session field. The scope is
// ALWAYS the session the viewer's CheckInteract gated, built from the URL
// path by the caller — see the package doc.
type queryArgs struct {
	// Tags narrows to entries carrying ALL of these; empty means no tag filter.
	Tags []string `json:"tags,omitempty"`
	// IDs selects specific entries by id; empty means no id filter.
	IDs []string `json:"ids,omitempty"`
	// Since is the inclusive lower time bound; nil means unbounded.
	Since *string `json:"since,omitempty"` // RFC3339
	// Until is the exclusive upper time bound; nil means unbounded.
	Until *string `json:"until,omitempty"`
	// Limit caps the entries returned; 0 means the backend's own default.
	Limit int `json:"limit,omitempty"`
}

// Resolve issues a read-only memory.Query for one binding. Order, all
// fail-closed: a nil Memory collaborator errors before anything else; the
// ref must name a registered, non-append-only Kind (the tamper-evident
// audit kinds — transcript, authz-decision, tool-session, audit — are never
// readable from a presentation binding, derived from Kind.Retention()
// rather than a transcribed list); the args must decode as the closed
// queryArgs shape; and the result is refused, not truncated, when it would
// exceed toolguard.DefaultUIIngressBytes.
func (Resolver) Resolve(ctx context.Context, d uibindings.Deps, req uibindings.Request) (uibindings.Result, error) {
	logger := d.Logger()
	session := req.Namespace + "/" + req.Session

	// Memory() returns an INTERFACE: a construction site assigning a typed-nil
	// pointer into it makes this check LIE (see uibindings.Deps). The guard is
	// honest only because the caller keeps the field a genuine nil.
	mem := d.Memory()
	if mem == nil {
		logger.Info("ui data binding memory resolve failed: Memory is not configured",
			"session", session, "path", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("uibindings/memory: Memory is not configured")
	}

	k, ok := memory.LookupKind(req.Ref)
	if !ok {
		logger.Info("ui data binding memory resolve failed: unregistered kind",
			"session", session, "path", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("this view's data source is not available")
	}
	if k.Retention().AppendOnly {
		// The tamper-evident audit ledger's sanctioned readers are the
		// session-view page and `oap audit verify`, not a presentation
		// binding — same "not available" copy as an unregistered kind so a
		// browser can't distinguish "doesn't exist" from "exists but is
		// off-limits".
		logger.Info("ui data binding memory resolve failed: kind is append-only",
			"session", session, "path", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("this view's data source is not available")
	}

	var args queryArgs
	if len(req.Args) > 0 {
		dec := json.NewDecoder(bytes.NewReader(req.Args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			logger.Info("ui data binding memory resolve failed: malformed args",
				"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
			return uibindings.Result{}, errors.New("this view's query is not valid")
		}
	}

	q := memory.Query{
		Scope: memory.Scope{Kind: "session", ID: session},
		Kinds: []string{req.Ref},
		IDs:   args.IDs,
		Tags:  args.Tags,
		Limit: args.Limit,
	}
	if args.Since != nil {
		t, err := time.Parse(time.RFC3339, *args.Since)
		if err != nil {
			logger.Info("ui data binding memory resolve failed: malformed since",
				"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
			return uibindings.Result{}, errors.New("this view's query is not valid")
		}
		q.Since = &t
	}
	if args.Until != nil {
		t, err := time.Parse(time.RFC3339, *args.Until)
		if err != nil {
			logger.Info("ui data binding memory resolve failed: malformed until",
				"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
			return uibindings.Result{}, errors.New("this view's query is not valid")
		}
		q.Until = &t
	}

	res, err := mem.Query(ctx, q)
	if err != nil {
		logger.Info("ui data binding memory resolve failed: query error",
			"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}

	contents := make([]json.RawMessage, len(res.Entries))
	for i, e := range res.Entries {
		contents[i] = e.Content
	}
	value, err := json.Marshal(contents)
	if err != nil {
		logger.Info("ui data binding memory resolve failed: encode result",
			"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}
	if int64(len(value)) > toolguard.DefaultUIIngressBytes {
		logger.Info("ui data binding memory resolve refused: result over the UI ingress ceiling",
			"session", session, "path", req.Path, "ref", req.Ref,
			"bytes", len(value), "limit", toolguard.DefaultUIIngressBytes)
		return uibindings.Result{}, errors.New(tooLargeMessage)
	}

	return uibindings.Result{Value: value}, nil
}
