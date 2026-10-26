// Package links parses oap:// links delivered to OAP Desktop (for example from
// the docs site) and holds the registry of link actions the app understands.
// It is platform-neutral and performs no I/O.
package links

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// MaxLen is the longest link, in bytes, Parse will look at.
const MaxLen = 2048

var (
	// ErrInvalid means the string is not a well-formed OAP link.
	ErrInvalid = errors.New("not a valid OAP link")
	// ErrUnsupported means the link is well-formed but names an action or
	// version this build does not know; a newer OAP Desktop may.
	ErrUnsupported = errors.New("link needs a newer version of OAP Desktop")
)

// Spec describes one supported link action.
type Spec struct {
	Key        string   // "<action>/<object>", e.g. "install/agent"
	MaxVersion int      // highest v= this build understands
	Params     []string // allowed params, each required exactly once
}

// Link is a parsed, validated link.
type Link struct {
	Key     string
	Version int
	Params  map[string]string
	Raw     string
}

var registry = map[string]Spec{}

// Register adds a link action. It panics on a duplicate key and is meant to be
// called from init only; the registry is not safe for concurrent mutation.
func Register(s Spec) {
	if _, dup := registry[s.Key]; dup {
		panic("links: duplicate registration for " + s.Key)
	}
	registry[s.Key] = s
}

// Keys returns the registered action keys, sorted.
func Keys() []string {
	keys := make([]string, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Parse validates raw and returns the link. Errors wrap ErrInvalid or
// ErrUnsupported.
func Parse(raw string) (Link, error) {
	if len(raw) > MaxLen {
		return Link{}, fmt.Errorf("%w: longer than %d bytes", ErrInvalid, MaxLen)
	}
	if strings.Contains(raw, "#") {
		return Link{}, fmt.Errorf("%w: fragment not allowed", ErrInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Link{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if u.Scheme != "oap" {
		return Link{}, fmt.Errorf("%w: scheme must be oap", ErrInvalid)
	}
	if u.Opaque != "" || u.User != nil || u.Host == "" {
		return Link{}, fmt.Errorf("%w: expected oap://<action>/<object>", ErrInvalid)
	}
	key := u.Host + u.Path
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Link{}, fmt.Errorf("%w: query: %v", ErrInvalid, err)
	}
	vs := q["v"]
	if len(vs) != 1 {
		return Link{}, fmt.Errorf("%w: v must appear exactly once", ErrInvalid)
	}
	v, err := strconv.Atoi(vs[0])
	if err != nil || v < 1 || strconv.Itoa(v) != vs[0] {
		return Link{}, fmt.Errorf("%w: v must be a positive integer", ErrInvalid)
	}
	spec, ok := registry[key]
	if !ok {
		return Link{}, fmt.Errorf("%w: unknown action %q", ErrUnsupported, key)
	}
	if v > spec.MaxVersion {
		return Link{}, fmt.Errorf("%w: %s v=%d", ErrUnsupported, key, v)
	}
	params := make(map[string]string, len(spec.Params))
	for name, vals := range q {
		if name == "v" {
			continue
		}
		if !slices.Contains(spec.Params, name) {
			return Link{}, fmt.Errorf("%w: unexpected parameter %q", ErrInvalid, name)
		}
		if len(vals) != 1 || vals[0] == "" {
			return Link{}, fmt.Errorf("%w: parameter %q must appear once and be non-empty", ErrInvalid, name)
		}
		params[name] = vals[0]
	}
	for _, name := range spec.Params {
		if _, ok := params[name]; !ok {
			return Link{}, fmt.Errorf("%w: missing parameter %q", ErrInvalid, name)
		}
	}
	return Link{Key: key, Version: v, Params: params, Raw: raw}, nil
}
