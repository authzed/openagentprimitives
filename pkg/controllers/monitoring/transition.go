// Package monitoring watches framework CR status conditions and
// publishes a channelevents.MonitoringEvent on every transition into or
// out of a failure state.
package monitoring

import (
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// tracker remembers, per (object, condition) key, whether the condition
// was last observed in its failure state. It is the in-memory backbone
// of the transitions-only reporting policy.
type tracker struct {
	mu   sync.Mutex
	seen map[string]bool // key → currently-failing
}

func newTracker() *tracker {
	return &tracker{seen: map[string]bool{}}
}

// observe records the latest failure-state for key and reports whether a
// transition occurred. When emit is true, transition is "failed" or
// "recovered"; otherwise it is empty. terminal comes from the Rule (see
// Rule.Terminal) and suppresses only the cold-start re-announce.
//
// The state is recorded either way — a suppressed cold observation must still
// seed the tracker, or the next tick would read it as a fresh transition.
func (t *tracker) observe(key string, isFailing, terminal bool) (emit bool, transition string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, known := t.seen[key]
	t.seen[key] = isFailing
	return detectTransition(known, prev, isFailing, terminal)
}

// forget drops every key with the given prefix. Called when a source
// object is deleted (prefix "<ns>/<name>/") so a later recreate is
// evaluated from a clean slate rather than emitting a spurious recovery.
func (t *tracker) forget(keyPrefix string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.seen {
		if strings.HasPrefix(k, keyPrefix) {
			delete(t.seen, k)
		}
	}
}

// detectTransition is the pure decision function. known reports whether
// the key has been observed before; prev is its previous failure-state
// (meaningful only when known is true).
//
// A first observation that is already failing counts as a none→failed
// transition: this re-announces open problems exactly once on operator
// restart, ensuring a failure that began during downtime is not missed.
//
// UNLESS the rule is terminal (Rule.Terminal), in which case a cold failing
// observation is a past incident on an object that will carry it forever, and
// announcing it says nothing about the cluster's current health. Transitions
// seen while running are unaffected — only the cold observation is silent.
func detectTransition(known, prev, isFailing, terminal bool) (emit bool, transition string) {
	if !known {
		if isFailing && !terminal {
			return true, channelevents.MonitoringTransitionFailed
		}
		return false, ""
	}
	if prev == isFailing {
		return false, ""
	}
	if isFailing {
		return true, channelevents.MonitoringTransitionFailed
	}
	return true, channelevents.MonitoringTransitionRecovered
}
