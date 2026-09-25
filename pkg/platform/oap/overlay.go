package oap

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Answers maps a question name to its answer (string, bool, int64, []string, …).
type Answers map[string]any

// Apply overlays non-secret answers onto crs per each question's bindings.
// Secret-typed questions are skipped (they drive Secret creation elsewhere).
// An unanswered question leaves its CR sentinel default untouched. A binding
// whose target CR is absent from crs is an error (fail-closed).
func Apply(crs []*unstructured.Unstructured, qs []Question, ans Answers) error {
	idx := indexByKindName(crs)
	for _, q := range qs {
		if q.Type == QSecret {
			continue
		}
		v, ok := ans[q.Name]
		if !ok {
			continue
		}
		for _, b := range q.Binding {
			t, err := ParseTarget(b.Target)
			if err != nil {
				return fmt.Errorf("question %q: %w", q.Name, err)
			}
			obj := idx[t.Kind+"/"+t.Name]
			if obj == nil {
				return fmt.Errorf("question %q: target %s/%s not found among bundle manifests", q.Name, t.Kind, t.Name)
			}
			if err := setAtPath(obj.Object, t.Path, normalize(v)); err != nil {
				return fmt.Errorf("question %q (%s): %w", q.Name, b.Target, err)
			}
		}
	}
	return nil
}

func indexByKindName(crs []*unstructured.Unstructured) map[string]*unstructured.Unstructured {
	m := make(map[string]*unstructured.Unstructured, len(crs))
	for _, o := range crs {
		m[o.GetKind()+"/"+o.GetName()] = o
	}
	return m
}

// discriminatorFor returns the field used to select a list element for a given
// list field. Defaults to "name"; list fields with a different key are listed
// explicitly. Extend this map as new selector-bearing fields are bound.
func discriminatorFor(field string) string {
	switch field {
	// "boundEntities" is the pre-4a spelling of "slots". It is kept so an older
	// bundle still overlays cleanly and then fails on the AgentClass's explicit
	// rename error, rather than failing here as an unfindable list element.
	case "slots", "boundEntities":
		return "resourceType"
	default:
		return "name"
	}
}

// normalize converts Go-native slice types into the []any / scalar forms the
// unstructured tree requires.
func normalize(v any) any {
	switch s := v.(type) {
	case []string:
		out := make([]any, len(s))
		for i, e := range s {
			out[i] = e
		}
		return out
	case int:
		return int64(s)
	default:
		return v
	}
}

// setAtPath walks node along path and sets value at the final segment.
func setAtPath(node map[string]any, path []Segment, value any) error {
	cur := node
	for i, seg := range path {
		last := i == len(path)-1
		if seg.Append {
			// "field[]": union the answer list onto the field's existing list,
			// preserving the CR's statically-declared entries first and skipping
			// duplicates so a re-install is a no-op (not a growing list). Append
			// is only meaningful as the terminal segment.
			if !last {
				return fmt.Errorf("append marker %q[] must be the final path segment", seg.Field)
			}
			add, ok := value.([]any)
			if !ok {
				return fmt.Errorf("append marker %q[] requires a list answer, got %T", seg.Field, value)
			}
			merged, _ := cur[seg.Field].([]any) // nil when the field is unset
			seen := make(map[any]bool, len(merged))
			for _, e := range merged {
				seen[e] = true
			}
			for _, e := range add {
				if !seen[e] {
					merged = append(merged, e)
					seen[e] = true
				}
			}
			cur[seg.Field] = merged
			return nil
		}
		if seg.Selector == "" {
			if last {
				cur[seg.Field] = value
				return nil
			}
			next, ok := cur[seg.Field].(map[string]any)
			if !ok {
				return fmt.Errorf("path element %q is not a map", seg.Field)
			}
			cur = next
			continue
		}
		listRaw, ok := cur[seg.Field].([]any)
		if !ok {
			return fmt.Errorf("path element %q is not a list", seg.Field)
		}
		if last {
			return fmt.Errorf("selector %q[%q] cannot be the final path segment", seg.Field, seg.Selector)
		}
		disc := discriminatorFor(seg.Field)
		var elem map[string]any
		for _, item := range listRaw {
			mm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, _ := mm[disc].(string); s == seg.Selector {
				if elem != nil {
					return fmt.Errorf("multiple %s elements with %s=%q", seg.Field, disc, seg.Selector)
				}
				elem = mm
			}
		}
		if elem == nil {
			return fmt.Errorf("no %s element with %s=%q", seg.Field, disc, seg.Selector)
		}
		cur = elem
	}
	return nil
}
