package extract

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	mu     sync.RWMutex
	byMIME = map[string]Extractor{}
)

// Register claims every MIME in e.MIMEs(). It panics on a duplicate claim:
// two extractors silently competing for one type would make which one runs
// depend on package-import order, which is not a thing to debug in production.
func Register(e Extractor) {
	mu.Lock()
	defer mu.Unlock()
	for _, m := range e.MIMEs() {
		m = normalize(m)
		if prior, dup := byMIME[m]; dup {
			panic(fmt.Sprintf("extract: %q claimed by both %T and %T", m, prior, e))
		}
		byMIME[m] = e
	}
}

// For returns the extractor claiming mime. Parameters are ignored, so
// "text/plain; charset=utf-8" resolves to the "text/plain" backend.
func For(mime string) (Extractor, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := byMIME[normalize(mime)]
	return e, ok
}

// Supported lists every claimed MIME, sorted. Used by extractord's readiness
// output and by tests asserting the registered set.
func Supported() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(byMIME))
	for m := range byMIME {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func normalize(m string) string {
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	return strings.ToLower(strings.TrimSpace(m))
}
