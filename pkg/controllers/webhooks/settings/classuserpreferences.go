package settings

import (
	"bytes"
	"fmt"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ClassUserPreferencesError validates classUserPreferences SHAPE only —
// non-empty class names, keys, and values. It deliberately does NOT check
// keys against class schemas: a global may legitimately precede its class's
// install; the settings controller cross-checks once both exist. The cluster
// tier may not carry the field at all.
func ClassUserPreferencesError(spec *v1.SettingsSpec, isCluster bool) string {
	if len(spec.ClassUserPreferences) == 0 {
		return ""
	}
	if isCluster {
		return "classUserPreferences is namespace tier only (set it on AgentSettings)"
	}
	for class, prefs := range spec.ClassUserPreferences {
		if class == "" {
			return "empty class name"
		}
		for key, g := range prefs {
			if key == "" {
				return fmt.Sprintf("class %q: empty preference key", class)
			}
			if len(g.Value.Raw) == 0 {
				return fmt.Sprintf("class %q, key %q: value is required", class, key)
			}
			if bytes.Equal(bytes.TrimSpace(g.Value.Raw), []byte("null")) {
				return fmt.Sprintf("class %q, key %q: value must not be null", class, key)
			}
		}
	}
	return ""
}
