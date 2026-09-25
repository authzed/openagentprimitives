// Package config is the resource-projector layer behind the admin UI's
// Config phase. Each first-party "config" CRD (Agent, MCPServer, Toolkit,
// Channel, …) registers a Projector that lists its CRs and flattens each one
// into a presentation-agnostic ResourceRow. A single generic admind endpoint
// (GET /admin/v1/config/{resource}) dispatches by the URL slug onto the
// registered Projector, so the API surface grows by registration, not by a
// per-CRD handler.
//
// The registry mirrors the project's many string-keyed "kind" registries
// (channel kinds, tool kinds, …): a package-private *kindregistry.Registry
// with thin Register/Get/All forwarders. T29 registers the real projectors via
// blank import; this package only owns the contract + the machinery.
package config

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// Badge is a small key/value chip rendered next to a resource row (e.g.
// {"kind","github_pat"} or {"mode","strict"}). Presentation-agnostic — the
// UI decides styling.
type Badge struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Count is a labeled integer rollup for a row (e.g. {"sessions", 3} or
// {"tools", 12}).
type Count struct {
	Label string `json:"label"`
	Value int    `json:"value"`
}

// ResourceRow is the flattened, transport-shaped view of a single config CR.
// Projectors translate their CRD into this common shape so the UI renders
// every config kind through one table component.
type ResourceRow struct {
	// Name is the CR's metadata.name.
	Name string `json:"name"`
	// Namespace is empty for cluster-scoped resources.
	Namespace string `json:"namespace,omitempty"`
	// Scope is "cluster" or "namespaced".
	Scope string `json:"scope"`
	// Status is a short word the UI color-codes (Valid/Ready/Degraded/Unknown…).
	Status string `json:"status"`
	// StatusReason is the machine reason / short human note behind Status.
	StatusReason string `json:"statusReason,omitempty"`
	// Badges are small key/value chips (kind, mode, …).
	Badges []Badge `json:"badges,omitempty"`
	// Counts are labeled rollups (sessions, tools, …).
	Counts []Count `json:"counts,omitempty"`
	// ManageCmd is the `oap …` / `kubectl …` string an operator runs to change
	// this resource — the admin UI is read-only, so it shows the command.
	ManageCmd string `json:"manageCmd,omitempty"`
}

// Projector lists one config CRD's CRs and flattens each into a ResourceRow.
// Resource() is the URL slug the generic endpoint dispatches on (e.g.
// "agents"). List reads through the operator's cached client — every config
// CRD is operator-watched, so a cached List is correct here.
type Projector interface {
	Resource() string
	List(ctx context.Context, c client.Client) ([]ResourceRow, error)
}

// reg is the process-wide resource-projector registry. The exported funcs
// below are thin forwarders so consumers keep a stable API while the storage,
// mutexing, sorting, and panic-on-dup live in pkg/x/kindregistry.
var reg = kindregistry.New[Projector]("config-projectors", Projector.Resource)

// Register adds a projector under its Resource() slug. Panics on an empty or
// duplicate slug — both are init()-time programmer errors.
func Register(p Projector) { reg.Register(p) }

// Get returns the projector registered for resource, and whether one exists.
func Get(resource string) (Projector, bool) { return reg.Get(resource) }

// All returns every registered projector, sorted by slug.
func All() []Projector { return reg.All() }

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }
