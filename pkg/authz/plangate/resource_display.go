package plangate

import (
	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
)

// ResourceDisplay is how a resource TYPE says its instances should present on
// an approval card. A package-local mirror of
// v1alpha1.SpiceDBResourceDisplay rather than that type itself: CardInput is
// what pkg/authz/hooks builds, and that package deliberately does not import
// pkg/apis/v1alpha1 (see PlanGateDeps.SlotStanding's own doc) to avoid
// coupling the gate to the CRD types. runner.ResourceDisplaysOf is the one
// place that converts the status field into this shape.
type ResourceDisplay struct {
	// Name is the human name for the resource TYPE — shown when no
	// per-instance label can be derived.
	Name string
	// Icon names a glyph from the closed registry (knownIcon). An
	// unrecognized name is dropped by resolveResourceLine, never carried
	// through to the wire.
	Icon string
	// Label names a deriver from the closed registry (DeriveLabel).
	Label string
}

// labelDerivers is the CLOSED set of names a SpiceDBResourceDisplay.Label may
// reference — exactly the shape authz.transformRegistry already uses for
// value-keying transforms. Not a template and not a regex: every entry is a
// function pkg/authz/resourcedisplay authored, so a declaration can shorten
// an instance's presentation but can never fabricate one.
//
// The functions themselves live in pkg/authz/resourcedisplay because the
// admin console's Directory panel derives the same names from the same ids;
// this map stays here because the NAMES are a plangate/CRD vocabulary, and a
// card and a console row selecting a deriver by the same string is a
// coincidence, not a contract.
var labelDerivers = map[string]func(string) string{
	"url_path":     resourcedisplay.DeriveURLPath,
	"b64url_path":  resourcedisplay.DeriveB64URLPath,
	"last_segment": resourcedisplay.DeriveLastSegment,
	// "none" is a legitimate, explicit declaration meaning "never derive a
	// label for this type; always fall back to Name" — distinct from an
	// unrecognized name only in that it IS one of the names an author may
	// write, not in what it does (both derive nothing).
	"none": func(string) string { return "" },
}

// DeriveLabel turns a resource instance's raw value into a short display
// label using the named deriver. An unrecognized name derives nothing —
// there is no way for a declaration to produce a label this repo did not
// author.
func DeriveLabel(deriver, raw string) string {
	fn, ok := labelDerivers[deriver]
	if !ok {
		return ""
	}
	return fn(raw)
}

// knownIcons is the CLOSED, code-defined registry SpiceDBResourceDisplay.Icon
// may reference. Grows by row as new resource types need a mark — see the
// design's "Out of scope" note. "repository" is the generic mark a
// host-agnostic type (git_repo) declares; "github" is GitHub's own mark,
// legitimate only for a type that is never anything but a GitHub resource
// (github_repo).
var knownIcons = map[string]bool{
	"repository": true,
	"github":     true,
}

// knownIcon returns name unchanged if it is registered, or "" otherwise — an
// unrecognized icon name renders nothing, never a fallback image and never a
// guess, enforced here so a bad name never even reaches the wire.
func knownIcon(name string) string {
	if knownIcons[name] {
		return name
	}
	return ""
}

// resolveResourceLine computes a resource-instance CardLine's Text, Icon and
// Href from its type's declared display (if any, looked up by resourceType)
// and its raw instance value.
//
// resourceType is both the map key and the FALLBACK Text — today's exact
// behavior from before this field existed, preserved for any type that never
// declared a Display. Everything past that point — the derived label, the
// icon, the link — is additive and only ever activates for a type that opted
// in.
//
// The label is derived from rawID, never taken from anywhere an agent could
// have written prose: DeriveLabel runs a closed, code-authored function over
// the instance value, so the result is a transformation of agent-authored
// data, never agent-authored text standing in for one.
func resolveResourceLine(displays map[string]ResourceDisplay, resourceType, rawID string) (text, icon, href string) {
	text = resourceType
	d, ok := displays[resourceType]
	if !ok {
		return text, "", ""
	}
	if label := DeriveLabel(d.Label, rawID); label != "" {
		text = label
	} else if d.Name != "" {
		text = d.Name
	}
	return text, knownIcon(d.Icon), resourcedisplay.EligibleHref(rawID)
}
