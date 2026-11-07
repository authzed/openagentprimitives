package linkinstall

import (
	"fmt"
	"sync"
	"time"
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
	// RejectPending: a link is already held for the end of bring-up. The
	// first one keeps its place; a later one never silently displaces it.
	RejectPending
	// RejectCoolingDown: the user cancelled a link install moments ago.
	RejectCoolingDown
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
	case RejectPending:
		return "reject-pending"
	case RejectCoolingDown:
		return "reject-cooling-down"
	}
	return fmt.Sprintf("Decision(%d)", int(d))
}

// Gate serializes link installs, holds at most one link (the first) while the
// cluster starts, and refuses links for a short while after the user cancels
// one. It is safe for concurrent use; the zero value is ready.
type Gate struct {
	mu            sync.Mutex
	pending       string
	busy          bool
	cooldownUntil time.Time

	// Now is the clock for the cancel cooldown; nil means time.Now. Tests
	// set it to step time without sleeping.
	Now func() time.Time
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Offer decides what to do with link given the cluster's readiness. Busy wins
// over every readiness, then a cancel cooldown, then an already-held link.
func (g *Gate) Offer(link string, r Readiness) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy {
		return RejectBusy
	}
	if g.now().Before(g.cooldownUntil) {
		return RejectCoolingDown
	}
	switch r {
	case Ready:
		g.busy = true
		return RunNow
	case Starting:
		if g.pending != "" {
			return RejectPending
		}
		g.pending = link
		return Held
	default:
		return RejectUnavailable
	}
}

// CoolDown refuses every offered link for d. Called when the user cancels an
// install's approval dialog, so a page that fires a link again right away
// cannot put the dialog back under a user who is clicking to dismiss it.
func (g *Gate) CoolDown(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until := g.now().Add(d); until.After(g.cooldownUntil) {
		g.cooldownUntil = until
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

// Pending reports the held link without claiming or clearing it — for
// diagnostics, e.g. logging a link that is held behind a busy gate.
func (g *Gate) Pending() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending, g.pending != ""
}
