// Package envfallback resolves an env var that has been renamed, while a
// deprecated old name still works.
package envfallback

import "os"

// Get resolves newName, falling back to oldName. It returns
// os.Getenv(newName) when that is non-empty; else os.Getenv(oldName) with
// usedDeprecated=true when that is non-empty; else ("", false). Get never
// logs — a caller that gets usedDeprecated=true is responsible for surfacing
// the deprecation through its own logger.
func Get(newName, oldName string) (value string, usedDeprecated bool) {
	if v := os.Getenv(newName); v != "" {
		return v, false
	}
	if v := os.Getenv(oldName); v != "" {
		return v, true
	}
	return "", false
}
