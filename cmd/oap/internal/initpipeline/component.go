// Package initpipeline declares the declarative Component model used by the
// `oap init` install pipeline.  Downstream tasks (ASK phase, executor, renderer)
// depend on the exact field names and types defined here.
package initpipeline

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
)

// Resolved maps each Input.Flag name to the value the user supplied (or its
// default).  The ASK phase builds it and Resolve returns it to the caller that
// declared the Inputs, which is the one thing that knows what the answers mean.
type Resolved map[string]string

// rowLabel returns the label to show in the progress checklist row: DisplayName
// when set, Name otherwise.
func (c Component) rowLabel() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

// Input describes a single user-facing flag that a Component requires before
// it can be installed.
type Input struct {
	// Flag is the CLI flag name (without leading dashes).
	Flag string
	// Prompt is the human-readable question displayed in interactive mode.
	Prompt string
	// Default is the value used when the user supplies nothing.
	Default string
	// Required means installation fails if the value remains empty.
	Required bool
	// AskWhen, when non-nil, decides whether this Input is asked at all, from
	// the answers resolved so far: the flags the caller seeded, plus every
	// Input declared ahead of this one. Returning false asks nothing, leaves
	// the Input at its Default, and suppresses the Required check — an input a
	// run has decided it does not want cannot also be one it insists on.
	//
	// It exists for an input whose necessity is itself an answer. An ACME
	// account email is needed only once an external hostname is in hand and no
	// existing certificate issuer was named, and that hostname may be typed
	// into a screen ahead of it in the same run — so the condition cannot be
	// settled by the caller before the run starts.
	AskWhen func(Resolved) bool
}

// WaitSpec drives the post-install readiness loop for a Component.
type WaitSpec struct {
	// Poll reports whether the component is ready.
	Poll progress.Poll
	// Diagnose gathers a best-effort explanation when the wait stalls.
	Diagnose progress.Diagnose
	// ETA is the expected time-to-ready on a healthy, warm node. It drives the
	// one-time "taking longer than expected" soft-warn and the "~Xm expected"
	// hint in the progress row — it is NOT the give-up point. Size it as an
	// honest expectation; Deadline is what decides failure.
	ETA time.Duration
	// Deadline is the hard give-up window: how long the executor waits before
	// declaring the component failed (or, interactively, offering to keep
	// waiting). Zero derives it from ETA — see waitDeadline. Set it explicitly
	// only when a component's worst case is not a simple multiple of its
	// expected case.
	Deadline time.Duration
}

// BuildSpec lists the container images that must be built (or loaded) before
// this Component's manifests are applied.  Image-building detail is deferred
// to Plan 2; the executor uses this field to trigger builds.
type BuildSpec struct {
	Images []string
}

// Component is a declarative descriptor for one installable unit of the `oap
// init` pipeline (e.g. "operator", "postgres", "graphiti").
type Component struct {
	// Name is the unique identifier for this component (used in DependsOn
	// references and progress output).
	Name string
	// DisplayName is the human-readable label shown in the progress checklist
	// row. Falls back to Name when empty.
	DisplayName string
	// Optional components are skipped when the corresponding env/flag is absent;
	// they do not cause installation to fail.
	Optional bool
	// DependsOn lists Names of Components whose manifests must be applied and
	// whose readiness (Wait) must complete before this component's own apply and
	// wait begin. Both APPLY and WAIT phases defer this component into a later
	// topological wave than any listed dependency. For example, "webd-gateway"
	// lists "cert-manager" so cert-manager is applied and ready before the
	// gateway's manifests land.
	DependsOn []string
	// Inputs declares the user-facing flags the ASK phase must resolve.
	Inputs []Input
	// Build, when non-nil, describes images to build before applying Manifests.
	Build *BuildSpec
	// Secrets, when non-nil, is called before ANY component's Manifests are
	// applied, to write prerequisite Kubernetes Secrets.
	//
	// Neither Secrets nor Manifests is handed the ASK phase's Resolved answers,
	// and that narrowness is the deliberate choice rather than an omission. Every
	// component this pipeline runs builds its bytes from what the caller already
	// knows, and Resolve's caller reads the returned map itself
	// (cmd/oap/internal/installcmd/routing_ask.go). A component that genuinely
	// needs a typed answer should widen these signatures then, against its own
	// requirement — which is also what settles the open question of where the
	// ASK has to run relative to the component list. Passing every closure an
	// argument nothing populates would instead offer each one a "" to read and
	// believe.
	Secrets func(ctx context.Context) error
	// Manifests returns the raw YAML/JSON bytes to kubectl-apply for this
	// Component.  Each element is applied as a single document.
	Manifests func() ([][]byte, error)
	// Wait, when non-nil, drives the readiness poll loop after manifests are
	// applied.
	Wait *WaitSpec
	// Verify is an optional post-ready sanity check (e.g. a connectivity probe).
	Verify func(ctx context.Context) error
}
