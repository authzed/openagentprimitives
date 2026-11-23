package sessionschedule

import (
	"fmt"
	"time"
)

// EventWindow applies quiet hours without moving the event's original deadline.
// A false result means deferral would leave no time to run. The caller retains
// that skipped event so a later retry cannot grant it a new deadline.
func EventWindow(timezone string, hours []QuietHours, ready, deadline time.Time) (Window, bool, error) {
	if ready.IsZero() || !deadline.After(ready) || deadline.Sub(ready) > MaxHorizon || len(hours) > 14 {
		return Window{}, false, fmt.Errorf("event window requires a finite deadline")
	}
	spec := Spec{Timezone: timezone, QuietHours: hours}
	loc, err := spec.location()
	if err != nil {
		return Window{}, false, err
	}
	windows, err := compileQuietHours(spec)
	if err != nil {
		return Window{}, false, err
	}
	due := ready
	for due.Before(deadline) && quiet(windows, due, loc) {
		due = due.Truncate(time.Minute).Add(time.Minute)
	}
	if !due.Before(deadline) {
		return Window{}, false, nil
	}
	expiry := deadline
	if len(hours) > 0 {
		for cursor := due.Truncate(time.Minute).Add(time.Minute); cursor.Before(expiry); cursor = cursor.Add(time.Minute) {
			if quiet(windows, cursor, loc) {
				expiry = cursor
				break
			}
		}
	}
	return Window{DueAt: due.UTC(), ExpiresAt: expiry.UTC()}, true, nil
}
