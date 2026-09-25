package manifests

import (
	"bytes"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// Tags carries the optional image overrides for oap install. Registry, when set,
// prefixes every first-party image with "<registry>/". Operator/Runner are
// explicit per-image overrides that win over the registry prefix. Digests maps
// an image Name to its "sha256:…" digest for digest pinning on remote installs.
// The actual image list and resolution live in pkg/platform/apimage (single source of
// truth).
type Tags struct {
	Operator string
	Runner   string
	Registry string
	// Digests maps an image Name (e.g. "agentprimitives-runner") to its
	// "sha256:…" digest. When set (remote installs), each image's reference is
	// pinned to <reg>/<name>:<Version>@<digest>. Empty → registry-tag refs.
	Digests map[string]string
}

// Substitute rewrites every first-party image reference in the rendered YAML to
// its resolved form. Resolution (override vs digest vs registry vs local) is
// delegated to apimage.ResolveDigests; here we just byte-replace each
// <name>:dev string with its final reference. Each original is replaced once,
// so a registry prefix, digest pin, and a per-image override never double-apply.
func Substitute(in []byte, t Tags) ([]byte, error) {
	overrides := map[string]string{}
	if t.Operator != "" {
		overrides[apimage.Operator.Name] = t.Operator
	}
	if t.Runner != "" {
		overrides[apimage.Runner.Name] = t.Runner
	}
	out := in
	for orig, final := range apimage.ResolveDigests(t.Registry, t.Digests, overrides) {
		if final != orig {
			out = bytes.ReplaceAll(out, []byte(orig), []byte(final))
		}
	}
	return out, nil
}
