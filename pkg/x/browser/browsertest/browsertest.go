// Package browsertest is the one seam a test uses to observe or drive browser
// opening. It exists so that packages stop growing a package-level opener var
// and a SetOpen… setter apiece: five of those had accumulated, each a recorder
// re-implemented slightly differently, and each one a thing a new test could
// forget to install.
//
// Forgetting is already safe — browser.Open suppresses itself inside a test
// binary and returns browser.ErrSuppressed. What these helpers add is the
// ability to ASSERT which URL a flow would have opened, and to DRIVE a flow
// that only makes progress when the browser calls back (the OAuth consent and
// channel-handoff paths both do).
//
// Every helper restores the previous opener through t.Cleanup, so a test cannot
// leak an opener into the test that runs after it.
//
// The installed opener is process-wide. Tests using these helpers must not run
// t.Parallel() alongside each other — the same constraint the package vars they
// replace carried, now stated in one place instead of five.
package browsertest

import (
	"sync"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/x/browser"
)

// Recorder captures the URLs a flow asked to open.
type Recorder struct {
	mu   sync.Mutex
	urls []string
}

// install wires fn in for the duration of the test and returns rec.
func install(t *testing.T, rec *Recorder, fn func(string) error) *Recorder {
	t.Helper()
	t.Cleanup(browser.SetOpenerForTest(fn))
	return rec
}

// Record installs an opener that records each URL and reports success. Use it
// when the test wants to assert what a flow WOULD have opened.
func Record(t *testing.T) *Recorder {
	t.Helper()
	rec := &Recorder{}
	return install(t, rec, func(u string) error {
		rec.add(u)
		return nil
	})
}

// Fail installs an opener that records each URL and reports err, for the tests
// that exercise what a flow says when no browser could be launched — the
// headless, SSH and CI story, which is the path most likely to rot unread.
func Fail(t *testing.T, err error) *Recorder {
	t.Helper()
	rec := &Recorder{}
	return install(t, rec, func(u string) error {
		rec.add(u)
		return err
	})
}

// Use installs fn, recording each URL before handing it over. It is for a test
// that must DRIVE the flow — an OAuth consent page whose callback is what lets
// the flow finish — where returning without acting would hang the run.
func Use(t *testing.T, fn func(string) error) *Recorder {
	t.Helper()
	rec := &Recorder{}
	return install(t, rec, func(u string) error {
		rec.add(u)
		return fn(u)
	})
}

func (r *Recorder) add(u string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, u)
}

// URLs returns every URL opened so far, in order.
func (r *Recorder) URLs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

// Count returns how many opens were attempted. Distinguishing "opened once"
// from "opened on every retry" is a claim a bare URL comparison cannot make.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.urls)
}

// Last returns the most recent URL, or "" if nothing was opened.
func (r *Recorder) Last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.urls) == 0 {
		return ""
	}
	return r.urls[len(r.urls)-1]
}
