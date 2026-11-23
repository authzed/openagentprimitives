// Package sessionschedule resolves finite session schedules. It has no goal,
// identity, transport, or authorization dependencies: callers must separately
// approve the resolved windows, persist them, and authorize each activation.
package sessionschedule

import (
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
	"github.com/robfig/cron/v3"
)

const MaxRuns = 100
const MaxHorizon = 30 * 24 * time.Hour

type Spec struct {
	Kind             string       `json:"kind"`
	Timezone         string       `json:"timezone"`
	IntervalSeconds  int64        `json:"intervalSeconds,omitempty"`
	Weekdays         []int        `json:"weekdays,omitempty"`
	MaxRuns          int          `json:"maxRuns"`
	RunWindowSeconds int64        `json:"runWindowSeconds"`
	QuietHours       []QuietHours `json:"quietHours,omitempty"`
}

// Weekdays use Sunday=0. For overnight windows the weekday belongs to the
// starting date. An empty list applies every day; start is inclusive, end exclusive.
type QuietHours struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Weekdays []int  `json:"weekdays,omitempty"`
}

type Window struct {
	DueAt     time.Time `json:"dueAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Policy generates nominal slots in strictly increasing order, within [start,end).
// It must respect MaxRuns; Resolve independently checks these bounds. Quiet hours
// and execution windows are applied centrally, so a new policy cannot bypass them.
type Policy interface {
	Kind() string
	Slots(Spec, time.Time, time.Time, *time.Location) ([]time.Time, error)
	Describe(Spec) string
}

var policies = kindregistry.New[Policy]("session schedules", func(p Policy) string { return p.Kind() })

func Kinds() []string { return policies.Keys() }

func Register(p Policy) { policies.Register(p) }
func init() {
	Register(once{})
	Register(interval{})
	Register(calendar{kind: "daily"})
	Register(calendar{kind: "weekly"})
}

func clockMinutes(value string) (int, error) {
	t, err := time.Parse("15:04", value)
	if err != nil || t.Format("15:04") != value {
		return 0, fmt.Errorf("invalid local time %q; use HH:MM", value)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func validWeekdays(days []int) bool {
	seen := map[int]bool{}
	for _, d := range days {
		if d < 0 || d > 6 || seen[d] {
			return false
		}
		seen[d] = true
	}
	return true
}

func (s Spec) location() (*time.Location, error) {
	if s.Timezone == "" || s.Timezone == "Local" {
		return nil, fmt.Errorf("schedule requires an explicit IANA timezone")
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return nil, fmt.Errorf("schedule timezone: %w", err)
	}
	return loc, nil
}

func (s Spec) validate(start, end time.Time) (*time.Location, Policy, error) {
	if start.IsZero() || !end.After(start) || end.Sub(start) > MaxHorizon || start.Nanosecond() != 0 ||
		s.MaxRuns < 1 || s.MaxRuns > MaxRuns || s.RunWindowSeconds < 1 || s.RunWindowSeconds > 86400 || len(s.QuietHours) > 14 || !validWeekdays(s.Weekdays) {
		return nil, nil, fmt.Errorf("schedule requires 1–100 runs, whole-second start, a finite window, and at most 30 days")
	}
	loc, err := s.location()
	if err != nil {
		return nil, nil, err
	}
	p, ok := policies.Get(s.Kind)
	if !ok {
		return nil, nil, fmt.Errorf("unknown session schedule %q", s.Kind)
	}

	return loc, p, nil
}

type quietWindow struct {
	start, end int
	weekdays   []int
}

func compileQuietHours(spec Spec) ([]quietWindow, error) {
	var result []quietWindow
	for _, q := range spec.QuietHours {
		start, err := clockMinutes(q.Start)
		if err != nil {
			return nil, err
		}
		end, err := clockMinutes(q.End)
		if err != nil {
			return nil, err
		}
		if start == end || !validWeekdays(q.Weekdays) {
			return nil, fmt.Errorf("quiet hours require distinct start/end and valid weekdays")
		}
		result = append(result, quietWindow{start: start, end: end, weekdays: q.Weekdays})
	}
	return result, nil
}

func quiet(windows []quietWindow, t time.Time, loc *time.Location) bool {
	local := t.In(loc)
	m := local.Hour()*60 + local.Minute()
	day := int(local.Weekday())
	for _, q := range windows {
		applies := func(d int) bool { return len(q.weekdays) == 0 || slices.Contains(q.weekdays, d) }
		if q.start < q.end {
			if m >= q.start && m < q.end && applies(day) {
				return true
			}
		} else if (m >= q.start && applies(day)) || (m < q.end && applies((day+6)%7)) {
			return true
		}
	}
	return false
}

// Resolve is deterministic and independent of the current time. Nominal slots
// falling in quiet hours defer to the first allowed minute; collisions coalesce.
// A run window ends at the next quiet period or series end, whichever comes first.
// Expired windows must be skipped by the caller, never replayed as catch-up work.
func Resolve(s Spec, start, end time.Time) ([]Window, error) {
	loc, p, err := s.validate(start, end)
	if err != nil {
		return nil, err
	}
	quietWindows, err := compileQuietHours(s)
	if err != nil {
		return nil, err
	}
	slots, err := p.Slots(s, start, end, loc)
	if err != nil {
		return nil, err
	}
	if len(slots) > s.MaxRuns {
		return nil, fmt.Errorf("schedule policy exceeded run limit")
	}
	var result []Window
	var previous time.Time
	var lastDeferred time.Time
	for _, slot := range slots {
		if slot.Before(start) || !slot.Before(end) || (!previous.IsZero() && !slot.After(previous)) {
			return nil, fmt.Errorf("schedule policy returned an invalid slot")
		}
		previous = slot
		due := slot
		if due.Before(lastDeferred) {
			due = lastDeferred
		}
		for due.Before(end) && quiet(quietWindows, due, loc) {
			due = due.Truncate(time.Minute).Add(time.Minute)
		}
		lastDeferred = due
		if !due.Before(end) {
			continue
		}
		if len(result) > 0 && result[len(result)-1].DueAt.Equal(due) {
			continue
		}
		expiry := due.Add(time.Duration(s.RunWindowSeconds) * time.Second)
		if expiry.After(end) {
			expiry = end
		}
		if len(s.QuietHours) > 0 {
			for cursor := due.Truncate(time.Minute).Add(time.Minute); cursor.Before(expiry); cursor = cursor.Add(time.Minute) {
				if quiet(quietWindows, cursor, loc) {
					expiry = cursor
					break
				}
			}
		}
		result = append(result, Window{DueAt: due.UTC(), ExpiresAt: expiry.UTC()})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("schedule has no executable windows before its end")
	}
	return result, nil
}

func Describe(s Spec) (string, error) {
	p, ok := policies.Get(s.Kind)
	if !ok {
		return "", fmt.Errorf("unknown session schedule %q", s.Kind)
	}
	return p.Describe(s), nil
}

func QuietDescription(s Spec) string {
	var parts []string
	for _, q := range s.QuietHours {
		text := q.Start + "–" + q.End
		if len(q.Weekdays) > 0 {
			var days []string
			for _, d := range q.Weekdays {
				days = append(days, time.Weekday(d).String())
			}
			text += " (" + strings.Join(days, ", ") + ")"
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "; ") + " · " + s.Timezone
}

type once struct{}

func (once) Kind() string         { return "once" }
func (once) Describe(Spec) string { return "Once" }
func (once) Slots(s Spec, start, end time.Time, loc *time.Location) ([]time.Time, error) {
	if s.MaxRuns != 1 || s.IntervalSeconds != 0 || len(s.Weekdays) > 0 {
		return nil, fmt.Errorf("once schedule requires maxRuns=1 and no interval or weekdays")
	}
	return []time.Time{start}, nil
}

type interval struct{}

func (interval) Kind() string { return "interval" }
func (interval) Describe(s Spec) string {
	return "Every " + (time.Duration(s.IntervalSeconds) * time.Second).String()
}

func (interval) Slots(s Spec, start, end time.Time, loc *time.Location) ([]time.Time, error) {
	if s.IntervalSeconds < 60 || s.IntervalSeconds > int64(MaxHorizon/time.Second) || len(s.Weekdays) > 0 {
		return nil, fmt.Errorf("interval requires 60–2592000 seconds and no weekdays")
	}
	var slots []time.Time
	for t := start; t.Before(end) && len(slots) < s.MaxRuns; t = t.Add(time.Duration(s.IntervalSeconds) * time.Second) {
		slots = append(slots, t)
	}
	return slots, nil
}

type calendar struct{ kind string }

func (p calendar) Kind() string { return p.kind }
func (p calendar) Describe(s Spec) string {
	if p.kind == "daily" {
		return "Daily"
	}
	var days []string
	for _, d := range s.Weekdays {
		days = append(days, time.Weekday(d).String())
	}
	return "Weekly on " + strings.Join(days, ", ")
}

func (p calendar) Slots(s Spec, start, end time.Time, loc *time.Location) ([]time.Time, error) {
	if s.IntervalSeconds != 0 || (p.kind == "daily" && len(s.Weekdays) > 0) || (p.kind == "weekly" && len(s.Weekdays) == 0) {
		return nil, fmt.Errorf("calendar schedule requires no interval; weekly requires weekdays")
	}
	day := "*"
	if p.kind == "weekly" {
		var days []string
		for _, d := range s.Weekdays {
			days = append(days, fmt.Sprint(d))
		}
		day = strings.Join(days, ",")
	}
	local := start.In(loc)
	parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	expr, err := parser.Parse(fmt.Sprintf("CRON_TZ=%s %d %d %d * * %s", s.Timezone, local.Second(), local.Minute(), local.Hour(), day))
	if err != nil {
		return nil, err
	}
	var slots []time.Time
	lastDate := ""
	for t := expr.Next(start.Add(-time.Second)); !t.IsZero() && t.Before(end) && len(slots) < s.MaxRuns; t = expr.Next(t) {
		date := t.In(loc).Format("2006-01-02")
		// Missing local times are skipped by cron. A repeated wall time during
		// fall-back occurs once, at its first eligible instant.
		if date == lastDate {
			continue
		}
		lastDate = date
		slots = append(slots, t.UTC())
	}
	return slots, nil
}
