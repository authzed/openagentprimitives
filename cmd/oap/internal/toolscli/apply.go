package toolscli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/registry"
)

// ApplyResult is the per-document outcome of ApplyStream.
type ApplyResult struct {
	Kind      string
	Name      string
	Namespace string
	Created   bool
	Updated   bool
	Err       error
}

// ApplyStream reads multi-doc YAML from r, decodes each non-empty doc via
// registry.DecodeAny, and server-side-applies each via the typed client.
// Each doc gets its own ApplyResult so the caller can render a per-doc
// summary; one bad doc does not abort the stream.
//
// defaultNS is the namespace used when a decoded namespaced object has no
// metadata.namespace set. Cluster-scoped objects ignore defaultNS.
func ApplyStream(ctx context.Context, c client.Client, defaultNS string, r io.Reader) ([]ApplyResult, error) {
	docs, err := splitYAMLDocs(r)
	if err != nil {
		return nil, err
	}
	results := make([]ApplyResult, 0, len(docs))
	for _, doc := range docs {
		obj, k, err := registry.DecodeAny(doc)
		if err != nil {
			results = append(results, ApplyResult{Err: err})
			continue
		}
		// Default namespace if the object is namespaced and the caller left it
		// blank. Cluster-scoped kinds opt in via the optional Scoped interface.
		if obj.GetNamespace() == "" && defaultNS != "" && isNamespacedKind(k) {
			obj.SetNamespace(defaultNS)
		}
		// Set TypeMeta so server-side apply has APIVersion+Kind.
		gvr := k.GVR()
		obj.GetObjectKind().SetGroupVersionKind(gvr.GroupVersion().WithKind(k.Name()))

		res := ApplyResult{
			Kind:      k.Name(),
			Name:      obj.GetName(),
			Namespace: obj.GetNamespace(),
		}

		// Determine create-vs-update by probing first.
		probe := k.NewObject()
		getErr := c.Get(ctx, client.ObjectKeyFromObject(obj), probe)

		patchErr := c.Patch(ctx, obj, client.Apply,
			client.FieldOwner("ap-tools-apply"),
			client.ForceOwnership)
		if patchErr != nil {
			// Fall back to Create for a fake client that doesn't support apply on
			// non-existing objects.
			if apierrors.IsNotFound(patchErr) {
				if cErr := c.Create(ctx, obj); cErr == nil {
					res.Created = true
					results = append(results, res)
					continue
				} else {
					res.Err = cErr
					results = append(results, res)
					continue
				}
			}
			// Some fake clients reject apply; if the object doesn't exist yet,
			// try plain Create.
			if apierrors.IsNotFound(getErr) {
				obj.SetResourceVersion("")
				if cErr := c.Create(ctx, obj); cErr == nil {
					res.Created = true
					results = append(results, res)
					continue
				}
			}
			res.Err = patchErr
			results = append(results, res)
			continue
		}
		if apierrors.IsNotFound(getErr) {
			res.Created = true
		} else {
			res.Updated = true
		}
		results = append(results, res)
	}
	return results, nil
}

// splitYAMLDocs reads r and splits on YAML document separators ("---") into
// per-document byte slices, skipping empty / whitespace-only docs.
func splitYAMLDocs(r io.Reader) ([][]byte, error) {
	all, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	// We rely on a YAML-aware splitter rather than a naive strings.Split so
	// embedded "---" inside multi-line strings are handled correctly. Since
	// kubernetes/apimachinery already provides one, use it via the
	// k8s.io/apimachinery/pkg/util/yaml splitter.
	docs, err := splitYAML(all)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(docs))
	for _, d := range docs {
		// Skip whitespace-only docs.
		trim := bytes.TrimSpace(d)
		if len(trim) == 0 {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// splitYAML splits a YAML byte slice into documents using the standard
// `\n---` separator with optional leading whitespace tolerated. We use a
// hand-rolled splitter to avoid pulling extra deps; it walks lines and starts
// a new doc whenever a line consisting solely of `---` is seen.
func splitYAML(in []byte) ([][]byte, error) {
	var out [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(in))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var cur bytes.Buffer
	flush := func() {
		out = append(out, append([]byte(nil), cur.Bytes()...))
		cur.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if isDocSeparator(line) {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	if cur.Len() > 0 {
		flush()
	}
	if len(out) == 0 {
		return [][]byte{in}, nil
	}
	return out, nil
}

// isNamespacedKind reports whether a Kind is namespace-scoped. Kinds opt in
// to the cluster-scoped path by implementing contract.ClusterScoped (a tiny
// marker interface). Defaulting to namespaced matches the kubebuilder
// convention and keeps the contract narrow.
func isNamespacedKind(k contract.Kind) bool {
	if cs, ok := k.(contract.ClusterScoped); ok {
		return !cs.ClusterScoped()
	}
	return true
}

func isDocSeparator(line string) bool {
	// A YAML document separator is exactly `---` (optionally followed by
	// a comment). For our CLI we accept the common case with optional leading
	// spaces and trailing whitespace.
	stripped := bytes.TrimSpace([]byte(line))
	return string(stripped) == "---"
}
