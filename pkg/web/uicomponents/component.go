// Package uicomponents is the agent-UI component vocabulary: the closed set of
// renderable types an AgentUI declaration (author-written) or a view-model
// (agent-written) may name.
//
// The vocabulary is deliberately platform-owned. A view-model is LLM output,
// and what makes it safe is validation against a schema the PLATFORM controls:
// the model can only select from this set and bind to sources it was already
// authorized for. It can never introduce script, widen a CSP, or open an
// egress path.
//
// A component's Props struct is both the schema and the validator: Validate
// unmarshals into a fresh copy with DisallowUnknownFields, and Schema emits the
// JSON Schema handed to the agent. Keeping them the same artifact means they
// cannot drift.
package uicomponents

import "github.com/authzed/openagentprimitives/pkg/web/uicomponents/component"

// Component describes one renderable type in the vocabulary. An alias for
// component.Component, which lives in a leaf package so
// pkg/web/uicomponents/registry can depend on the concrete type without
// importing this package back — validate.go calls registry.Get, so the reverse
// import would be a cycle. The alias keeps the split invisible to callers.
type Component = component.Component

// ParamValue is one runtime parameter key a control drives — aliased from the
// same leaf package, for the same reason Component is.
type ParamValue = component.ParamValue
