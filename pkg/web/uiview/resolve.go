package uiview

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// Resolve is THE read path for a session's current UI: the CR's Tier-0
// declaration with every stored Tier-1 fragment merged on and re-validated.
//
// Both the runner (deciding whether an update_view write would produce a legal
// document) and webd (on every browser-facing read) call it, so a fragment the
// runner accepted and a fragment webd serves can never be different things.
//
// A stored fragment whose node no longer PARSES is reported in View.Rejected
// alongside the ones that fail to validate, never skipped: both mean "this hook
// is showing Tier 0 and the agent thinks it is not", and the caller must log
// either one.
//
// ui must be non-nil; mem may not. A nil memory.Memory returns an error rather
// than an empty fragment list — "no backend" and "the agent has composed
// nothing" are different facts, and rendering the second when the first is true
// hides a broken deployment behind a working-looking page. (memory.Memory is an
// INTERFACE: the nil check is only honest because every construction site
// declares its variable as the interface — AGENTS.md's typed-nil rule.)
func Resolve(ctx context.Context, mem memory.Memory, scope memory.Scope,
	ui *spiceboxv1alpha1.AgentUI, o uicomponents.Options) (uicomponents.View, error) {
	if mem == nil {
		return uicomponents.View{}, fmt.Errorf("uiview: Resolve: no memory backend for scope %s/%s", scope.Kind, scope.ID)
	}

	base, err := DeclarationFromSpec(ui)
	if err != nil {
		return uicomponents.View{}, fmt.Errorf("uiview: Resolve: convert %s: %w", ui.Name, err)
	}

	stored, err := uiviewmodel.List(ctx, mem, scope, ui.Name)
	if err != nil {
		return uicomponents.View{}, fmt.Errorf("uiview: Resolve: list fragments for %s: %w", ui.Name, err)
	}

	fragments, parseRejections := loadFragments(stored)

	view := uicomponents.ResolveView(base, fragments, o)
	view.Rejected = append(view.Rejected, parseRejections...)
	slices.SortFunc(view.Rejected, func(a, b uicomponents.Rejection) int { return strings.Compare(a.Hook, b.Hook) })
	view.ComposedAt = composedAt(stored, view.AgentComposed)

	return view, nil
}

// composedAt computes View.ComposedAt: for each hook in composed, the latest
// WrittenAt among stored's records for that slot — the fill or clear that
// stands. uiviewmodel.Record replaces a slot's record in place (see its own
// doc comment), so in practice at most one stored record ever matches a
// given slot; the max is taken anyway so this stays correct even if that
// ever stopped being true, rather than silently assuming it.
//
// A name in composed with no matching entry in stored cannot happen in
// practice — ResolveView only adds a hook to AgentComposed after accepting
// the very fragment loadFragments built from stored — so this simply leaves
// it absent from the result rather than asserting on it.
func composedAt(stored []uiviewmodel.Content, composed []string) map[string]time.Time {
	if len(composed) == 0 {
		return nil
	}
	names := make(map[string]bool, len(composed))
	for _, h := range composed {
		names[h] = true
	}
	out := make(map[string]time.Time, len(composed))
	for _, c := range stored {
		if !names[c.Slot] {
			continue
		}
		if cur, ok := out[c.Slot]; !ok || c.WrittenAt.After(cur) {
			out[c.Slot] = c.WrittenAt
		}
	}
	return out
}

// loadFragments turns stored uiviewmodel.Content records into
// uicomponents.Fragment values fit for ResolveView. A Cleared record becomes a
// Fragment with a nil Node — an intentional empty — with no attempt to parse
// its (empty) Node; any other record's Node must still PARSE, and one that no
// longer does — the vocabulary shed a field between the write and this read —
// becomes a Rejection rather than being dropped. Shared by Resolve and
// Runtime.apply so neither turns a stored record into a Fragment its own way.
//
// BOTH callers must account for the rejections; discarding the second return
// leaves a hook serving Tier 0 with nobody told. Resolve appends them to
// View.Rejected for its caller to log; Runtime.apply (Write and Clear's shared
// body) appends them AND logs them itself, because its early return on an
// unaccepted proposal drops the View first.
func loadFragments(stored []uiviewmodel.Content) ([]uicomponents.Fragment, []uicomponents.Rejection) {
	var fragments []uicomponents.Fragment
	var rejections []uicomponents.Rejection
	for _, c := range stored {
		if c.Cleared {
			fragments = append(fragments, uicomponents.Fragment{Hook: c.Slot, Node: nil})
			continue
		}
		node, err := uicomponents.ParseNode(c.Node)
		if err != nil {
			rejections = append(rejections, uicomponents.Rejection{Hook: c.Slot, Reason: err.Error()})
			continue
		}
		fragments = append(fragments, uicomponents.Fragment{Hook: c.Slot, Node: &node})
	}
	return fragments, rejections
}
