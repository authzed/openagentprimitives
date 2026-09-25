// Package providers exposes the embedded provider YAMLs. Companion to
// pkg/platform/identity/provider, which provides the typed loader. We split
// embed-from-dir from typed-loading because go:embed only sees paths
// under the embedding package's own directory.
package providers

import "embed"

//go:embed *.yaml
var FS embed.FS
