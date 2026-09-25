package spec

import "fmt"

// PathKey builds the dotted rule path used in Generation.Descriptions,
// in exceptions[].overrides, and in validator Decision.FailedOn.Path.
// Pass index = -1 for non-indexed paths (e.g., "deny.effects.destructive").
func PathKey(section string, index int) string {
	if index < 0 {
		return section
	}
	return fmt.Sprintf("%s[%d]", section, index)
}
