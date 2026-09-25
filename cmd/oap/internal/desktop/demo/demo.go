// Package demo embeds the macOS desktop bundle's demo AgentClass — a no-tools
// `pirate-private` pirate-speak translator (defined in pirate.yaml) so the
// built-in web chat's agent selector has something to talk to as soon as
// bring-up finishes.
//
// The canonical copy lives here, under cmd/oap's own directory tree, rather
// than alongside the rest of the desktop bundle's build-time assets in
// build/desktop/ (where e.g. the k3s rootfs image sources live): Go's
// //go:embed directive cannot reach outside the embedding file's own module
// subtree (no "../" patterns), so a file under build/desktop/ could not be
// embedded from a source file in cmd/oap/... at all. This mirrors how
// pkg/platform/manifests, pkg/authz/spicedb/schema, and providers/toolkits each embed their
// own YAML from their own package directory.
package demo

import _ "embed"

// AgentClassYAML is the raw single-document AgentClass manifest applied by
// cmd/oap's desktop configureHook (see applyDemoAgentClass in
// cmd/oap/internal/desktopcmd/run_darwin.go). Kept as raw bytes here (rather
// than parsed into a typed object at package-init time) so a malformed embed
// surfaces as an ordinary parse error at the call site, not an init-time panic.
//
//go:embed pirate.yaml
var AgentClassYAML []byte
