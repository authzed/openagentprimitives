// Package actionstate is the "action" uibindings.Resolver: it answers "what
// is this declared action doing right now, for THIS viewer" out of the
// ui_action memory records the runner writes.
//
// A READ of a lifecycle, never an invoke: firing an action is
// POST .../actions, a different route with a different method and an origin
// pin. Resolve never dispatches to the runner, never touches channelevents,
// and refuses any binding with a non-empty args template — the call-time half
// of the rule uicomponents' validateActions enforces at declaration time. No
// binding at any source can cause a mutation: "writes are never a data
// binding".
//
// THE DISTINCTION THIS PACKAGE PROTECTS: an "action:" binding REFERENCES a
// declared action's lifecycle read-only; it is not the action. Referencing one
// from a Node.Binding — to gray out a control, show a spinner, render a custom
// pending display — is exactly what this resolver is for and is fully legal.
// "Never reachable from Node.Bindings" means never INVOCABLE through a
// binding. The two things a binding source may never do to an action are call
// its tool and accept caller-supplied args; this does neither.
package actionstate

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/registry"
)

// source is this resolver's Resolver.Source() value and the string a
// component prop's Binding declares to route here.
const source = "action"

// maxActionStateRecords bounds how many of the viewer's own ui_action
// records Resolve considers when hunting for the newest one naming this ref.
// Mirrors pkg/web/webui/agentui/live.go's maxLiveActions (64) — the same
// "a long-lived, click-happy session" justification applies identically to a
// single binding's lookback window.
const maxActionStateRecords = 64

// Resolver is the "action" uibindings.Resolver.
type Resolver struct{}

// New returns the "action" resolver.
func New() uibindings.Resolver { return Resolver{} }

func init() { registry.Register(New()) }

// Source implements uibindings.Resolver.
func (Resolver) Source() string { return source }

// Resolve returns the action's current lifecycle as a JSON STRING of
// browser-safe human copy (uiaction.DisplayCopy), not a structured object.
//
// A string is the right shape: an author binds an ap:alert's body to an
// action ref and gets "Waiting for your approval." A structured object would
// bind cleanly to nothing in the vocabulary — every bindable text prop is a
// string — so it would look more general while being less usable.
//
// An action with NO record yet is NOT an error. It resolves to DisplayCopy's
// copy for the zero-value State, the EMPTY STRING: this is what a Tier-0 page
// paints on first load, before any interaction, so a sentence here would be a
// status line about an action nothing has happened to, and it must never be a
// diagnostic since it goes straight in front of a viewer. The same fallback
// covers a record naming a DIFFERENT action or belonging to a DIFFERENT
// viewer — all three are indistinguishable from "nothing to report yet",
// which is the point: no case leaks another action's or viewer's state.
//
// Order, all fail-closed: nil Memory errors first; Args must be empty; the ref
// is matched against records for THIS viewer only (uiaction.RequesterKey on
// both sides) and THIS action only (Content.Action == req.Ref).
func (Resolver) Resolve(ctx context.Context, d uibindings.Deps, req uibindings.Request) (uibindings.Result, error) {
	logger := d.Logger()
	session := req.Namespace + "/" + req.Session

	// Memory() returns an INTERFACE: a construction site assigning a typed-nil
	// pointer into it makes this check LIE (see uibindings.Deps). The guard is
	// honest only because the caller keeps the field a genuine nil.
	mem := d.Memory()
	if mem == nil {
		logger.Info("ui action state resolve failed: Memory is not configured",
			"session", session, "path", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("uibindings/action: Memory is not configured")
	}

	// An action-state binding never takes arguments — nothing about "what is
	// this button doing" is parameterizable. The declaration-time validator
	// already rejects this; refusing again here means a template that should
	// never have existed is never substituted-and-ignored.
	if len(req.Args) > 0 {
		logger.Info("ui action state resolve failed: non-empty args template",
			"session", session, "path", req.Path, "ref", req.Ref)
		return uibindings.Result{}, errors.New("this view's control is not configured correctly")
	}

	records, err := uiaction.List(ctx, mem, memory.Scope{Kind: "session", ID: session},
		uiaction.RequesterKey(req.Subject), maxActionStateRecords)
	if err != nil {
		logger.Info("ui action state resolve failed: list error",
			"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's action state could not be loaded")
	}

	// List already scoped to THIS viewer, so the only filter left is this
	// ref's action name. Newest is chosen by explicit UpdatedAt comparison
	// rather than by trusting List's ordering, which holds only insofar as the
	// backend honors its OrderBy — a wrong pick silently shows a stale
	// lifecycle on a live control.
	var (
		state     uiaction.State
		addressed bool
		found     bool
		newestAt  time.Time
	)
	for _, c := range records {
		if c.Action != req.Ref {
			continue
		}
		if !found || c.UpdatedAt.After(newestAt) {
			state = c.State
			addressed = c.ApprovalAddressedToViewer
			newestAt = c.UpdatedAt
			found = true
		}
	}

	value, err := json.Marshal(uiaction.DisplayCopy(state, addressed))
	if err != nil {
		logger.Info("ui action state resolve failed: encode result",
			"session", session, "path", req.Path, "ref", req.Ref, "err", err.Error())
		return uibindings.Result{}, errors.New("this view's action state could not be loaded")
	}

	return uibindings.Result{Value: value}, nil
}
