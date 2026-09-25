// Package wstest scales the websocket read deadlines the pkg/web/webui tests
// set on a connection, so a loaded or instrumented box lengthens them instead
// of failing them.
//
// The deadlines these tests use are "long enough for a frame that is already
// on its way" — two or three seconds of slack around a sub-millisecond local
// round trip. That slack is generous against an idle machine and thin against
// `mage test:unit`, which runs every package in parallel under `-race`: the
// frame still arrives, just after the deadline, and the test fails with
// `read tcp ... i/o timeout`. Two ship gates each lost one such test on a
// loaded box, and each passed when re-run alone.
//
// So the base durations stay where they are — they are the right answer for
// the machine the author is sitting at — and this package widens them for the
// conditions that make them wrong.
//
// # Where it does NOT belong
//
// Only a read that waits for something that IS coming may scale. A negative
// read — one whose timeout IS the pass condition, "no frame arrives in this
// window" — must stay a short literal: scaling it buys nothing but wall-clock,
// and under a large scale factor it would turn a fast assertion into a stall.
// Those sites are marked with a comment where they appear.
//
// # This is a non-test package on purpose
//
// Four packages' tests need it (chat, artifactview, agentui, sessionview), and
// a `_test.go` file cannot be shared across packages. Nothing outside a test
// may import it; TestWstestIsImportedOnlyByTests enforces that structurally.
package wstest

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// TimeoutScaleEnv is the escape hatch for a box the built-in factors do not
// cover — an over-subscribed CI runner, a laptop with four other suites on it.
// Set it to a positive number to multiply every scaled deadline by that much:
//
//	AP_TEST_TIMEOUT_SCALE=4 mage test:unit
//
// It is a bare multiplier, not a duration: "4" and "1.5" are values, "4s" is
// not. A value that is not a positive number is ignored, with one line on
// stderr saying so — an operator who mistyped the knob they reached for
// because a suite was failing should not be told nothing and left believing
// it took effect.
const TimeoutScaleEnv = "AP_TEST_TIMEOUT_SCALE"

// Scale lengthens d by the race-detector factor (see factor_race.go) and by
// TimeoutScaleEnv. A non-positive d is returned untouched: zero means "no
// deadline" to net.Conn and a negative one is already expired, so neither is
// a duration to stretch.
func Scale(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return time.Duration(float64(d) * float64(raceFactor) * envFactor())
}

// Deadline returns an absolute read deadline base from now, scaled by Scale.
// It is the form nearly every call site wants:
//
//	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(2*time.Second)))
func Deadline(base time.Duration) time.Time {
	return time.Now().Add(Scale(base))
}

// warnOnce keeps a bad TimeoutScaleEnv value from printing on every one of the
// hundreds of reads a suite makes; the first line is the one that gets read.
var warnOnce sync.Once

// envFactor reads TimeoutScaleEnv on every call rather than caching it, so a
// test can set it with t.Setenv and see the effect.
func envFactor() float64 {
	raw, ok := os.LookupEnv(TimeoutScaleEnv)
	if !ok || raw == "" {
		return 1
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f <= 0 {
		warnOnce.Do(func() {
			fmt.Fprintf(os.Stderr,
				"wstest: ignoring %s=%q: want a positive number (a multiplier, e.g. 4 or 1.5), not a duration\n",
				TimeoutScaleEnv, raw)
		})
		return 1
	}
	return f
}
