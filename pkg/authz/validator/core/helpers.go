package core

import "maps"

// CopyAnyMap returns a shallow copy of src; nil/empty input returns nil
// (no allocation). Used by both validators to snapshot a parsed
// invocation's args/flags into the redaction-safe ParsedCall /
// ParsedArgs view without sharing storage with the original parser
// output.
func CopyAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	maps.Copy(out, src)
	return out
}
