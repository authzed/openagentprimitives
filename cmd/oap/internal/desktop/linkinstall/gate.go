package linkinstall

import (
	"fmt"
	"sync"
)

// Readiness is the local cluster's state as the caller observed it when a link
// arrived.
type Readiness int

const (
	// Ready means the cluster is running and can take an install now.
	Ready Readiness = iota
	// Starting means bring-up is in progress; a link may be held until it ends.
	Starting
	// Unavailable means the cluster is neither running nor starting.
	Unavailable
)

func (r Readiness) String() string {
	switch r {
	case Ready:
		return "ready"
	case Starting:
		return "starting"
	case Unavailable:
		return "unavailable"
	}
	return fmt.Sprintf("Readiness(%d)", int(r))
}

// Decision is what to do with an offered link.
type Decision int

const (
	// RunNow: install immediately; the gate is now busy until Release.
	RunNow Decision = iota
	// Held: the link is parked until the cluster finishes starting.
	Held
	// RejectUnavailable: the cluster is not running or starting.
	RejectUnavailable
	// RejectBusy: another link install is already in flight.
	RejectBusy
)

func (d Decision) String() string {
	switch d {
	case RunNow:
		return "run-now"
	case Held:
		return "held"
	case RejectUnavailable:
		return "reject-unavailable"
	case RejectBusy:
		return "reject-busy"
	}
	return fmt.Sprintf("Decision(%d)", int(d))
}

// Gate serializes link installs and holds at most one link (the newest) while
// the cluster starts. It is safe for concurrent use.
type Gate struct {
	mu      sync.Mutex
	pending string
	busy    bool
}

// Offer decides what to do with link given the cluster's readiness. Busy wins
// over every readiness. When a Starting offer replaces an older held link, the
// older one is returned as replaced ("" if none).
func (g *Gate) Offer(link string, r Readiness) (d Decision, replaced string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy {
		return RejectBusy, ""
	}
	switch r {
	case Ready:
		g.busy = true
		return RunNow, ""
	case Starting:
		replaced = g.pending
		g.pending = link
		return Held, replaced
	default:
		return RejectUnavailable, ""
	}
}

// Release marks the in-flight install finished (success or failure).
func (g *Gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.busy = false
}

// TakePending claims the held link after a successful bring-up. It returns
// ("", false) when nothing is held or an install is in flight; on true the
// gate is busy and the held link is cleared.
func (g *Gate) TakePending() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy || g.pending == "" {
		return "", false
	}
	link := g.pending
	g.pending = ""
	g.busy = true
	return link, true
}

// DropPending clears and returns the held link (bring-up failed or the cluster
// stopped). It does not touch busy.
func (g *Gate) DropPending() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == "" {
		return "", false
	}
	link := g.pending
	g.pending = ""
	return link, true
}
