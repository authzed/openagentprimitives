//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/health"
)

const (
	// healthRefreshInterval is how often watchHealth re-checks every registered
	// component while the cluster is running. 15s is frequent enough to notice a
	// crash-loop within a couple of ticks without hammering the guest apiserver.
	healthRefreshInterval = 15 * time.Second
	// healthRefreshTimeout bounds one full sweep of health.All() Checks.
	healthRefreshTimeout = 10 * time.Second
	// maxHealthMenuItems bounds the pre-created submenu pool. systray cannot
	// add/remove items at runtime, so the pool is fixed; it MUST be >=
	// len(health.All()) (guarded by TestMaxHealthMenuItems_CoversRegistry).
	maxHealthMenuItems = 10
)

// namedResult pairs a registered component's name with its health check result.
type namedResult struct {
	Name string
	Res  health.Result
}

// componentHealth is one rendered line for the status submenu.
type componentHealth struct {
	Name     string
	Healthy  bool // Status==OK. Absent (NotFound) components never appear; an unhealthy optional is Healthy:false + Optional:true.
	Optional bool // registry-optional: shown, but never alarms the icon/notifications.
	Detail   string
}

// classifyHealth turns raw registry results into (rendered submenu lines, the
// set of REQUIRED components that are unhealthy). NotFound results are dropped
// (the local profile simply doesn't deploy them). An Optional component that is
// unhealthy is shown (Optional:true) but never added to `unhealthy`, mirroring
// `oap check`'s warn-only treatment.
func classifyHealth(results []namedResult) (lines []componentHealth, unhealthy map[string]string) {
	unhealthy = map[string]string{}
	for _, nr := range results {
		if nr.Res.NotFound {
			continue
		}
		optional := nr.Res.Status == health.Optional
		healthy := nr.Res.Status == health.OK
		lines = append(lines, componentHealth{
			Name:     nr.Name,
			Healthy:  healthy,
			Optional: optional,
			Detail:   nr.Res.Detail,
		})
		if nr.Res.Status == health.Failed { // Failed is required-only; optional maps to health.Optional
			unhealthy[nr.Name] = nr.Res.Detail
		}
	}
	return lines, unhealthy
}

// healthDiff compares the previous and current required-unhealthy sets.
// newlyBad = components in cur but not prev (sorted, stable). recovered = the
// set went from non-empty to empty (fire a single "recovered" notification).
func healthDiff(prev, cur map[string]string) (newlyBad []string, recovered bool) {
	for name := range cur {
		if _, was := prev[name]; !was {
			newlyBad = append(newlyBad, name)
		}
	}
	sort.Strings(newlyBad)
	recovered = len(prev) > 0 && len(cur) == 0
	return newlyBad, recovered
}

// healthTitle renders the status-line title from the required-unhealthy set.
func healthTitle(unhealthy map[string]string) string {
	switch n := len(unhealthy); n {
	case 0:
		return "Running"
	case 1:
		return "1 component unhealthy"
	default:
		return fmt.Sprintf("%d components unhealthy", n)
	}
}

// healthLineLabel renders one submenu line: a status glyph, the component name,
// and its detail. Optional-unhealthy components are flagged so the reader knows
// the green icon is intentional.
func healthLineLabel(c componentHealth) string {
	switch {
	case c.Healthy:
		return fmt.Sprintf("✓ %s — %s", c.Name, c.Detail)
	case c.Optional:
		return fmt.Sprintf("⚠ %s — %s (optional)", c.Name, c.Detail)
	default:
		return fmt.Sprintf("✗ %s — %s", c.Name, c.Detail)
	}
}

// notification is one macOS notification to fire (pure value so the decision is
// testable without invoking notify()).
type notification struct {
	Title, Message string
}

// healthNotifications decides which notifications to fire for a health
// transition. Empty when disabled, or when nothing transitioned. Never fires
// on "stays bad" (only newly-bad and full-recovery), so a persistent failure
// notifies exactly once.
func healthNotifications(prev, cur map[string]string, enabled bool) []notification {
	if !enabled {
		return nil
	}
	newlyBad, recovered := healthDiff(prev, cur)
	var out []notification
	if len(newlyBad) > 0 {
		out = append(out, notification{
			Title:   "OAP Desktop — unhealthy",
			Message: strings.Join(newlyBad, ", ") + " not ready",
		})
	}
	if recovered {
		out = append(out, notification{Title: "OAP Desktop — recovered", Message: "all components healthy"})
	}
	return out
}

// watchHealth polls the cluster's component health on a ticker while the cluster
// is running, mirroring watchSessions. Stops on context cancellation.
func (s *desktopState) watchHealth() {
	ticker := time.NewTicker(healthRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			running := s.running
			s.mu.Unlock()
			if running {
				s.refreshHealth()
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// refreshHealth runs every registered component's Check, classifies the
// results, updates the submenu + icon, and fires transition-only
// notifications. Best-effort: a client/list error is logged and the last-known
// UI is left intact (a transient apiserver blip must not blank the submenu or
// false-alarm the icon).
func (s *desktopState) refreshHealth() {
	s.mu.Lock()
	dg := s.dg
	s.mu.Unlock()
	if dg == nil {
		return
	}
	b, err := dg.Bundle()
	if err != nil {
		s.logf("refresh health: build cluster client: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, healthRefreshTimeout)
	defer cancel()

	comps := health.All()
	results := make([]namedResult, 0, len(comps))
	for _, c := range comps {
		results = append(results, namedResult{Name: c.Name(), Res: c.Check(ctx, b)})
	}
	lines, unhealthy := classifyHealth(results)

	// Fire notifications for transitions BEFORE overwriting healthUnhealthy.
	s.mu.Lock()
	prev := s.healthUnhealthy
	s.healthUnhealthy = unhealthy
	s.mu.Unlock()

	// Read the notification preference OUTSIDE the lock (disk I/O) — a load
	// failure defaults to enabled and is logged, never silently dropped.
	enabled := true
	if cfg, err := desktop.Load(s.support); err != nil {
		s.logf("refresh health: load config: %v", err)
	} else {
		enabled = cfg.HealthNotificationsEnabled()
	}
	for _, n := range healthNotifications(prev, unhealthy, enabled) {
		notify(n.Title, n.Message)
	}

	s.applyHealth(lines, unhealthy)
}

// applyHealth rewires the submenu pool to `lines` and flips the icon between
// running and error based on `unhealthy`. The icon is only touched while the
// cluster is running, checked under mu so a racing Stop/Uninstall/Quit (which
// set running=false under the same lock) always wins — a stale health tick can
// never leave a stopped cluster showing "Running".
func (s *desktopState) applyHealth(lines []componentHealth, unhealthy map[string]string) {
	s.mu.Lock()
	pool := s.healthPool
	statusItem := s.statusItem
	anim := s.iconAnim
	running := s.running
	n := len(lines)
	if n > len(pool) {
		n = len(pool)
	}
	if running {
		if len(unhealthy) > 0 {
			if anim != nil {
				anim.SetState(menubaricons.StateError)
			}
		} else if anim != nil {
			anim.SetState(menubaricons.StateRunning)
		}
	}
	s.mu.Unlock()

	if statusItem != nil {
		statusItem.SetTitle(healthTitle(unhealthy))
	}
	for i, item := range pool {
		if i < n {
			item.SetTitle(healthLineLabel(lines[i]))
			item.Show()
		} else {
			item.Hide()
		}
	}
}

// setHealthNotifications persists the toggle to config.json (load-modify-save)
// and reflects it on the checkbox. Load-modify-save (rather than writing a
// fresh Config) preserves every other field the setup form owns.
func (s *desktopState) setHealthNotifications(enabled bool) {
	s.mu.Lock()
	support := s.support
	item := s.healthNotifyItem
	s.mu.Unlock()

	cfg, err := desktop.Load(support)
	if err != nil {
		s.logf("toggle health notifications: load config: %v", err)
		return
	}
	cfg.HealthNotifications = &enabled
	if err := cfg.Save(support); err != nil {
		s.logf("toggle health notifications: save config: %v", err)
		// systray already flipped the checkbox visually on click; the save
		// failed, so revert it to match the unchanged on-disk value rather than
		// leaving the menu out of sync with config.json until restart.
		if item != nil {
			if enabled {
				item.Uncheck()
			} else {
				item.Check()
			}
		}
		return
	}
	if item != nil {
		if enabled {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
}
