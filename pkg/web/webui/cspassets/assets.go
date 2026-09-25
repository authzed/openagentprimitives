// Package cspassets assembles the nonce-authorized <style>/<script> tags a page
// needs and derives the matching Content-Security-Policy from them. Callers
// register styles and scripts (and any extra CSP needs) without ever handling a
// nonce by hand — Render mints ONE nonce, stamps every tag with it, and grants
// script-src / style-src the nonce only for capabilities that are actually
// registered. Everything else stays denied by the page's default-src 'none', so
// the policy is automatically as tight as the page really is: turn a feature off
// (don't register its assets) and its CSP allowances vanish with it.
package cspassets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"strings"
)

// ErrEmptyDirective is returned by Render/RenderWith when a registered directive
// cannot be emitted: no name, or no source left once empty strings are dropped.
//
// The builder REFUSES such a directive rather than skipping it, because skipping
// is not the safer option: an empty source list is a CSP parse error, so the UA
// discards the directive, and frame-ancestors has no default-src fallback — a
// discarded one and an omitted one are the same outcome at the browser, any
// origin may frame the page. Nor can the builder substitute a safe value: what
// "deny" means is directive- and caller-specific ('none' for frame-ancestors,
// 'self' for style-src), and guessing wrong either opens the page or breaks it.
// So the caller chooses the fail-closed source (see hostPage in
// pkg/web/webui/artifactview/host.go) and the builder refuses what it cannot
// render.
var ErrEmptyDirective = errors.New("cspassets: directive has no name or no usable source")

// Dir is one CSP directive: a name plus its source list, e.g.
// {"frame-ancestors", ["https://shell.example"]}.
type Dir struct {
	Name    string
	Sources []string
}

// Directive is a terse constructor for a fixed page directive. It performs no
// validation of its own — a Dir is just data — but registering one with no
// name, or with no non-empty source, makes the Set unrenderable: see
// ErrEmptyDirective.
func Directive(name string, sources ...string) Dir { return Dir{Name: name, Sources: sources} }

// usableSources returns d's sources with empty strings dropped. An empty
// result means the directive cannot be emitted at all.
func (d Dir) usableSources() []string {
	out := make([]string, 0, len(d.Sources))
	for _, s := range d.Sources {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ScriptSpec is one <script> to emit. Src and Body are mutually exclusive (Src =
// external, Body = inline). Type ("module", "application/json", …) and ID are
// optional attributes; the nonce is added by Render.
type ScriptSpec struct {
	Type string
	ID   string
	Src  string
	Body string
}

// Set collects the nonce-authorized inline assets a page needs, its fixed CSP
// policy, and any extra per-item CSP needs. The zero value is ready to use.
//
// Registration is chainable and returns no error, so a directive the Set
// cannot render is recorded on err and surfaced by Render/RenderWith. First
// error wins: the point is to refuse the whole policy, not to report every
// bad directive.
type Set struct {
	policy  []Dir
	needs   []Dir
	styles  []string
	scripts []ScriptSpec
	err     error
}

// reject records the first registration error. Both render entry points check
// it, so a Set that cannot produce a complete policy never produces a partial
// one.
func (s *Set) reject(name string) {
	if s.err == nil {
		s.err = fmt.Errorf("%w: %q", ErrEmptyDirective, name)
	}
}

// checkDir records a rejection when d cannot be emitted as a real directive.
func (s *Set) checkDir(d Dir) {
	if d.Name == "" || len(d.usableSources()) == 0 {
		s.reject(d.Name)
	}
}

// Policy sets the page's fixed CSP directives (default-src, frame-src,
// frame-ancestors, …). script-src / style-src are added by Render from what's
// registered — do not set them here.
//
// A directive with no name or no non-empty source is refused (ErrEmptyDirective):
// the caller must supply its own fail-closed source, e.g. 'none'.
func (s *Set) Policy(dirs ...Dir) *Set {
	for _, d := range dirs {
		s.checkDir(d)
	}
	s.policy = append(s.policy, dirs...)
	return s
}

// Style registers an inline stylesheet (implies the style-src nonce).
func (s *Set) Style(css string) *Set { s.styles = append(s.styles, css); return s }

// AddScript registers a <script> from a full spec (type/id/src/body).
func (s *Set) AddScript(spec ScriptSpec) *Set { s.scripts = append(s.scripts, spec); return s }

// Script registers an inline script (implies the script-src nonce).
func (s *Set) Script(js string) *Set { return s.AddScript(ScriptSpec{Body: js}) }

// ScriptSrc registers an external script by URL (implies the script-src nonce,
// which authorizes the nonce'd external tag).
func (s *Set) ScriptSrc(src string) *Set { return s.AddScript(ScriptSpec{Src: src}) }

// Need declares an extra CSP source an item requires beyond the implicit nonce —
// e.g. a connect-src origin, or an extra script-src source. Merged into any
// same-named directive, de-duplicated.
//
// Refused on the same terms as Policy: a Need with no name or no non-empty
// source would either widen nothing or, when it names a directive nothing else
// registered, emit a valueless one.
func (s *Set) Need(name string, sources ...string) *Set {
	d := Dir{Name: name, Sources: sources}
	s.checkDir(d)
	s.needs = append(s.needs, d)
	return s
}

// Rendered is the output of Render: one nonce, the assembled <style>/<script>
// tag strings (split so styles go in <head> and scripts after the content they
// operate on), and the derived Content-Security-Policy header value.
type Rendered struct {
	Nonce   string
	Styles  string
	Scripts string
	CSP     string
}

// Render mints a fresh nonce and renders (see RenderWith). Use this when the page
// owns its nonce end-to-end; use RenderWith when the nonce is minted elsewhere
// (e.g. shared with a separately-built CSP).
func (s *Set) Render() (Rendered, error) {
	nonce, err := NewNonce()
	if err != nil {
		return Rendered{}, err
	}
	return s.RenderWith(nonce)
}

// RenderWith stamps every registered asset into nonce'd tags using the given
// nonce and derives the CSP. Inline bodies are NOT escaped — callers register
// only trusted, self-authored CSS/JS; external src and the type/id attributes are
// attribute-escaped.
//
// Returns ErrEmptyDirective when any registered directive cannot be emitted,
// rather than rendering a partial policy: this is the second door into csp(),
// and a caller that mints its own nonce must not walk past the guard.
func (s *Set) RenderWith(nonce string) (Rendered, error) {
	if s.err != nil {
		return Rendered{}, s.err
	}
	var st, sc strings.Builder
	for _, css := range s.styles {
		st.WriteString(`<style nonce="` + nonce + `">` + css + `</style>`)
	}
	for _, scr := range s.scripts {
		sc.WriteString(`<script`)
		if scr.Type != "" {
			sc.WriteString(` type="` + html.EscapeString(scr.Type) + `"`)
		}
		if scr.ID != "" {
			sc.WriteString(` id="` + html.EscapeString(scr.ID) + `"`)
		}
		if scr.Src != "" {
			sc.WriteString(` src="` + html.EscapeString(scr.Src) + `"`)
		}
		sc.WriteString(` nonce="` + nonce + `">`)
		if scr.Src == "" {
			sc.WriteString(scr.Body)
		}
		sc.WriteString(`</script>`)
	}
	return Rendered{Nonce: nonce, Styles: st.String(), Scripts: sc.String(), CSP: s.csp(nonce)}, nil
}

// csp assembles the policy: the fixed directives, then script-src / style-src
// granted the nonce only when a script / style is registered, then any Need()s —
// all merged by directive name with de-duplicated, order-preserving sources.
//
// Only reached once RenderWith has confirmed every registered directive is
// emittable, so the empty-source filtering below is belt-and-braces: it keeps a
// stray separator out of a directive mixing a real source with an empty one,
// which the registration check accepts.
func (s *Set) csp(nonce string) string {
	order := make([]string, 0, len(s.policy)+2+len(s.needs))
	byName := map[string][]string{}
	seen := map[string]bool{}
	add := func(name string, sources ...string) {
		if _, ok := byName[name]; !ok {
			order = append(order, name)
		}
		for _, src := range sources {
			if src == "" {
				continue
			}
			key := name + "\x00" + src
			if seen[key] {
				continue
			}
			seen[key] = true
			byName[name] = append(byName[name], src)
		}
	}

	for _, d := range s.policy {
		add(d.Name, d.Sources...)
	}
	if len(s.scripts) > 0 {
		add("script-src", "'nonce-"+nonce+"'")
	}
	if len(s.styles) > 0 {
		add("style-src", "'nonce-"+nonce+"'")
	}
	for _, d := range s.needs {
		add(d.Name, d.Sources...)
	}

	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, name+" "+strings.Join(byName[name], " "))
	}
	return strings.Join(parts, "; ")
}

// NewNonce returns a fresh base64url CSP nonce.
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
