package settingseditor

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Drift returns the sorted dotted JSON paths at which readback differs from
// intended. After a forced server-side apply of the full spec the two can
// still differ: SSA cannot remove a field another manager owns by omission,
// so any surviving path here is a field kept alive by a different field
// manager (or mutated by a webhook default). Callers surface these to the
// user rather than pretending the apply converged.
//
// The error path is defensive — SettingsSpec marshals cleanly — but the
// errors are returned per the repo's no-silently-dropped-errors rule.
func Drift(intended, readback *v1alpha1.SettingsSpec) ([]string, error) {
	ab, err := json.Marshal(intended)
	if err != nil {
		return nil, fmt.Errorf("marshal intended spec: %w", err)
	}
	bb, err := json.Marshal(readback)
	if err != nil {
		return nil, fmt.Errorf("marshal readback spec: %w", err)
	}
	var a, b map[string]any
	if err := json.Unmarshal(ab, &a); err != nil {
		return nil, fmt.Errorf("unmarshal intended spec: %w", err)
	}
	if err := json.Unmarshal(bb, &b); err != nil {
		return nil, fmt.Errorf("unmarshal readback spec: %w", err)
	}
	paths := map[string]struct{}{}
	diffValue("", a, b, paths)
	out := make([]string, 0, len(paths))
	for p := range paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func diffValue(path string, a, b any, out map[string]struct{}) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	// Treat nil as an empty map so we can recurse into nested structures
	if !aok && a == nil {
		am = map[string]any{}
		aok = true
	}
	if !bok && b == nil {
		bm = map[string]any{}
		bok = true
	}
	if aok && bok {
		keys := map[string]struct{}{}
		for k := range am {
			keys[k] = struct{}{}
		}
		for k := range bm {
			keys[k] = struct{}{}
		}
		for k := range keys {
			diffValue(joinPath(path, k), am[k], bm[k], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		if path == "" {
			path = "(root)"
		}
		out[path] = struct{}{}
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return fmt.Sprintf("%s.%s", prefix, key)
}
