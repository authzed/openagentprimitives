// Package kubeyaml splits a multi-document Kubernetes YAML (or JSON) stream
// into unstructured objects.
//
// It is deliberately a leaf: stdlib plus k8s.io/apimachinery, no repo imports.
// The shared home for this could not be pkg/platform/manifests — that package embeds the
// ~1MB generated install bundle, and internal/cmd/operator and internal/cmd/webd import the
// callers (pkg/platform/oap, pkg/platform/cloud) without importing pkg/platform/manifests, so hanging the
// splitter off manifests would pull the whole install.yaml into both binaries
// to reuse twenty lines.
package kubeyaml

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Split parses a multi-document YAML/JSON stream into individual unstructured
// objects.
//
// Documents that are not resources are skipped rather than returned: an empty
// or comment-only document decodes to an empty map, and a document with no
// `kind` is not a resource at all — there is no GroupVersionKind to apply,
// dispatch, or authorize on. Returning those to a caller pushes a
// "is this even a resource?" check onto every call site, and a caller that
// forgets it reports the empty Kind as if the author had written one.
func Split(in []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bufio.NewReader(bytes.NewReader(in)), 4096)
	var out []*unstructured.Unstructured
	for {
		raw := map[string]any{}
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, fmt.Errorf("kubeyaml.Split: decode: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: raw}
		if u.GetKind() == "" {
			continue
		}
		out = append(out, u)
	}
}
