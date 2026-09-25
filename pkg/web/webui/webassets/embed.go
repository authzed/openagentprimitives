// Package webassets embeds the built web UI bundles (pkg/web/webui/webassets/dist,
// produced by Vite under web/) and exposes the asset filesystem + parsed
// manifest. The dist tree is committed and rebuilt by `mage web:build`; CI's
// `mage web:check` fails on drift.
package webassets

import (
	"embed"
	"encoding/json"
	"io/fs"
	"strings"
)

//go:embed dist
var distFS embed.FS

// Entry is one app's built assets, keyed by appKey in the manifest.
type Entry struct {
	Scripts []string `json:"scripts"` // /assets URLs in load order; the entry module is first
	CSS     []string `json:"css"`     // /assets stylesheet URLs; empty when the app ships no CSS
}

// FS returns the embedded filesystem rooted at the module (paths start "dist/").
func FS() fs.FS { return distFS }

// Manifest parses dist/manifest.json into appKey -> Entry.
func Manifest() (map[string]Entry, error) {
	b, err := distFS.ReadFile("dist/manifest.json")
	if err != nil {
		return nil, err
	}
	var m map[string]Entry
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// trimAssetsPrefix turns a manifest URL ("/assets/x.js") into a dist-relative
// suffix ("/x.js") for opening against FS() ("dist"+suffix).
func trimAssetsPrefix(url string) string {
	return strings.TrimPrefix(url, "/assets")
}
