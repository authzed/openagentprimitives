package config

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// ResourceDetail is the resource-agnostic detail view behind the admin UI's
// per-resource detail page. It carries the Overview scalars (name, scope,
// status, description, manageCmd) plus a list of Sections the frontend renders
// as TABS. Like ResourceRow, it is presentation-agnostic: a DetailProjector
// flattens one CR into this common shape and the UI renders every config kind
// through one detail component.
type ResourceDetail struct {
	// Name is the CR's metadata.name (or a human label for opaque-named CRs
	// like UserIdentity — mirroring the list Projector's choice).
	Name string `json:"name"`
	// Namespace is empty for cluster-scoped resources.
	Namespace string `json:"namespace,omitempty"`
	// Scope is "cluster" or "namespaced".
	Scope string `json:"scope"`
	// Status is the same short word the list Projector produces
	// (Valid/Ready/Reachable/Connected/Degraded/Unknown).
	Status string `json:"status"`
	// StatusReason is the machine reason / short human note behind Status.
	StatusReason string `json:"statusReason,omitempty"`
	// Description is a human-readable summary for the Overview header.
	Description string `json:"description,omitempty"`
	// ManageCmd is the `kubectl …` string an operator runs to change this
	// resource — the admin UI is read-only, so it shows the command.
	ManageCmd string `json:"manageCmd,omitempty"`
	// Sections are the per-tab content blocks.
	Sections []Section `json:"sections,omitempty"`
}

// Section is one tab of a ResourceDetail. Kind selects which of the three
// content shapes is populated:
//   - "fields": Fields is a label/value list (with optional entity links).
//   - "text":   Text is a single free-form block (a system prompt, a SKILL.md
//     body, a health message).
//   - "list":   Items is a list of titled rows (with optional links + badges),
//     and Text — if set — is a NOTICE about the list, not content of its own.
//
// That last case is the one to read before setting Text on a list Section: the
// frontend renders it as a destructive alert above the rows (see
// SectionRenderer's ListSection). It exists so a backend that could read only
// PART of a list can say so, rather than presenting a short list as a complete
// one — directoryIdentitiesSection and directoryScopesSection are the
// projectors using it today, both over the same partial-SpiceDB-read shape
// (spicedb.UnavailableProbe). A projector that sets it for any other reason
// gets an alert it did not intend.
type Section struct {
	// ID is a stable slug the frontend keys the tab on (e.g. "prompt").
	ID string `json:"id"`
	// Label is the human tab label (e.g. "Prompt").
	Label string `json:"label"`
	// Kind is "fields" | "text" | "list".
	Kind string `json:"kind"`
	// Fields is populated when Kind == "fields".
	Fields []Field `json:"fields,omitempty"`
	// Text is the block content when Kind == "text". When Kind == "list" it is
	// instead an optional NOTICE about the list (rendered as an alert above the
	// rows) — see the type doc above before setting it there.
	Text string `json:"text,omitempty"`
	// Items is populated when Kind == "list".
	Items []ListItem `json:"items,omitempty"`
}

// Section kind discriminators.
const (
	SectionFields = "fields"
	SectionText   = "text"
	SectionList   = "list"
)

// Field is one label/value row in a "fields" Section. Link, when set, turns
// Value into a navigation link to another admin entity's detail page. Href,
// when set, turns Value into an external hyperlink to that (scheme-guarded)
// URL. When both are empty an http(s) Value is still auto-linked by the UI, so
// the field stays backward-compatible with the earlier raw-URL-value form.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// Link, when non-nil, points at another admin entity's detail page.
	Link *Link `json:"link,omitempty"`
	// Href, when non-empty, is an external hyperlink target the UI renders
	// behind Value's anchor text (scheme-guarded by the frontend). It lets a
	// field show a short human label (e.g. a short commit SHA) while linking the
	// full URL, instead of relying on the UI auto-linking a verbose raw-URL
	// Value. Internal entity links use Link; external URLs use Href.
	Href string `json:"href,omitempty"`
}

// Link references another admin entity's detail page. Entity is the UI's
// entity slug ("agent", "tool", "identity", "source", "skill", …); ID is the
// detail id ("<ns>/<name>" for namespaced entities, "<name>" for cluster).
type Link struct {
	Entity string `json:"entity"`
	ID     string `json:"id"`
}

// ListItem is one row in a "list" Section (a tool, a credential, a subcommand).
type ListItem struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Link     *Link  `json:"link,omitempty"`
	// Href, when non-empty, is an external hyperlink target the UI renders
	// behind Title's anchor text — the same split Field.Href makes, and for
	// the same reason: a row can show a short name (a repository's
	// `demo-org/widgets`) while linking the full URL it was derived from.
	// Internal entity links use Link; external URLs use Href. Link wins when
	// both are set.
	//
	// Scheme-guarded by the frontend, and also by whatever produced it: the
	// Scopes panel's hrefs come from resourcedisplay.EligibleHref, which is
	// https-only and returns its input unchanged.
	Href   string  `json:"href,omitempty"`
	Badges []Badge `json:"badges,omitempty"`
}

// DetailProjector flattens a single config CR into a ResourceDetail. Resource()
// is the SAME URL slug the list Projector registers under ("agents", "tools",
// …), so the generic detail endpoint dispatches on it identically. Detail reads
// through the operator's cached client (every config CRD is operator-watched).
//
// A CR that does not exist at (ns, name) is reported as (nil, nil) — the
// handler turns that into a 404. Any other read failure is returned as an
// error → 500. ns is "" for cluster-scoped resources.
type DetailProjector interface {
	Resource() string
	Detail(ctx context.Context, c client.Client, ns, name string) (*ResourceDetail, error)
}

// detailReg is the process-wide detail-projector registry, a sibling of reg
// (the list-projector registry). Keyed by the same Resource() slug.
var detailReg = kindregistry.New[DetailProjector]("config-detail-projectors", DetailProjector.Resource)

// RegisterDetail adds a detail projector under its Resource() slug. Panics on
// an empty or duplicate slug — both are init()-time programmer errors.
func RegisterDetail(p DetailProjector) { detailReg.Register(p) }

// GetDetail returns the detail projector registered for resource, and whether
// one exists.
func GetDetail(resource string) (DetailProjector, bool) { return detailReg.Get(resource) }

// AllDetail returns every registered detail projector, sorted by slug.
func AllDetail() []DetailProjector { return detailReg.All() }

// ResetDetail clears the detail registry. Test-only helper; do not call from
// production code paths.
func ResetDetail() { detailReg.Reset() }
