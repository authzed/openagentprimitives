// The two things a kind can say about a round trip that are not "here is
// where to send them": that this run does not need one at all, and that there
// is an address the operator has to visit before the fallback questions make
// sense.
//
// Neither is a failure, and keeping them out of the failure path is the point
// of both. A stand-down is the operator's own choice, so it must not be
// reported as "the browser step did not finish"; a fire-and-forget address has
// no callback to wait for, so nothing may block on it.
package channelwizard

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// browserLog records every address a run tried to open, and optionally refuses
// them — a browser that is not there is the case both degradation paths below
// are about.
type browserLog struct {
	mu     sync.Mutex
	opened []string
	err    error
}

func (b *browserLog) open(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened = append(b.opened, url)
	return b.err
}

func (b *browserLog) urls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.opened...)
}

// standDownSpec is a handoff the kind can decide, from the answers, not to
// make. Begin fails the test outright: a stand-down that still called it would
// have opened a listener and a browser for a round trip nobody wanted.
func standDownSpec(t *testing.T, reason string) *channelkinds.HandoffSpec {
	t.Helper()
	return &channelkinds.HandoffSpec{
		SkipWhen: func(answers map[string]string) string {
			if answers["app-source"] == "existing" {
				return reason
			}
			return ""
		},
		Begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
			t.Error("Begin must not be called for a run whose kind stood the round trip down")
			return channelkinds.HandoffStart{}, errors.New("unreachable")
		},
		Complete: func(context.Context, url.Values) (map[string]string, error) {
			t.Error("Complete must not be called for a run whose kind stood the round trip down")
			return nil, errors.New("unreachable")
		},
		FallbackInputs: []oap.Question{
			{Name: "app-id", Type: oap.QString, Prompt: "App ID"},
			{Name: "installation-id", Type: oap.QString, Prompt: "Installation ID"},
		},
	}
}

// TestRunHandoff_AKindThatStandsTheRoundTripDownIsAskedTheFallbackInstead is
// the mechanism a kind's own route question rides on. Wizard.Handoff is
// described BEFORE any question is answered, so a kind cannot decline the
// round trip by returning nil from it — the answer that settles the route does
// not exist yet. This is where it declines instead.
func TestRunHandoff_AKindThatStandsTheRoundTripDownIsAskedTheFallbackInstead(t *testing.T) {
	var browser browserLog
	run, st := handoffParams(t, standDownSpec(t, "you already have an App, so there is nothing to create"),
		browser.open, "12345\n67890\n")
	run.answers["app-source"] = "existing"
	// THE SCREEN IS ASSERTED ON, not only the run's notes. The wording that
	// distinguishes a stand-down from a failed round trip is carried on the
	// first question's Description — a note would land in the post-run
	// summary, after the questions it was meant to explain — so a test reading
	// only the notes cannot tell the two apart, and passes against a run that
	// tells the operator their own choice went wrong.
	screen := drivenScreen(t, &run, "12345\n67890\n")

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)

	assert.Empty(t, browser.urls(), "no browser opens for a round trip the kind declined to make")
	assert.Equal(t, "12345", answers["app-id"])
	assert.Equal(t, "67890", answers["installation-id"],
		"the fallback questions are what a stood-down handoff asks instead")

	read := screen.String()
	assert.Contains(t, read, "you already have an App",
		"the kind's own sentence is what stands above the questions that replace the round trip")
	assert.NotContains(t, strings.ToLower(read), "did not finish",
		"a deliberate stand-down is not a failed round trip and must not be reported to the operator as one")

	assert.Contains(t, strings.Join(noteValues(st), "\n"), "you already have an App",
		"and it is recorded for the post-run summary as well")
}

// drivenScreen re-points a run's driver at a buffer so a test can read what the
// operator was actually shown. handoffParams sends the rendered screens to
// io.Discard, which is right for the tests that only care about the answers
// collected and wrong for any test making a claim about WORDING.
//
// script is the same answer script handoffParams was given; the driver is
// rebuilt around it because a tui.Driver carries its own reader and writer and
// neither is reachable once built.
func drivenScreen(t *testing.T, run *handoffRun, script string) *bytes.Buffer {
	t.Helper()
	var shown bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	run.opts.Theme = theme
	run.opts.Driver = tui.Plain(strings.NewReader(script), &shown, theme)
	return &shown
}

// TestRunHandoff_AnUndecidedRunStillMakesTheRoundTrip is the regression
// direction: SkipWhen answering "" leaves the round trip exactly as it was.
func TestRunHandoff_AnUndecidedRunStillMakesTheRoundTrip(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	spec := demoHandoff(t, svc.URL)
	spec.SkipWhen = func(map[string]string) string { return "" }

	run, _ := handoffParams(t, spec, fakeBrowser(t, svc.URL, nil), "67890\n")
	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "from-abc", answers["app-id"], "the exchange still runs when the kind does not decline it")
}

// ---------------------------------------------------------------------------
// the fire-and-forget address
// ---------------------------------------------------------------------------

// openAfterSpec is a completed round trip that leaves the operator with
// somewhere to go: github's installation page, which GitHub never redirects
// back from because the App declares no Setup URL.
func openAfterSpec(t *testing.T, serviceURL, target string) *channelkinds.HandoffSpec {
	t.Helper()
	spec := demoHandoff(t, serviceURL)
	spec.FallbackOpenURL = func(answers map[string]string) string {
		if answers["app-id"] == "" {
			return ""
		}
		return target
	}
	return spec
}

// TestRunHandoff_OpensTheAddressTheKindNamesBeforeAskingTheFallback.
//
// It is opened BEFORE the questions because the screen takes the terminal
// over, and the answer to those questions is on the page being opened. And
// nothing waits on it: this address sends no callback, so a run that blocked
// on one would hang until the handoff timeout for a step that already
// succeeded.
func TestRunHandoff_OpensTheAddressTheKindNamesBeforeAskingTheFallback(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	const installURL = "https://github.com/apps/demo-reviewbot-gh/installations/new"
	var browser browserLog
	run, _ := handoffParams(t, openAfterSpec(t, svc.URL, installURL), func(u string) error {
		// The round trip's own browser is the fake one; anything else is the
		// fire-and-forget open under test.
		if strings.HasPrefix(u, "http://127.0.0.1:") {
			return fakeBrowser(t, svc.URL, nil)(u)
		}
		return browser.open(u)
	}, "67890\n")
	var printed bytes.Buffer
	run.opts.Out = &printed

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "67890", answers["installation-id"],
		"the run still ASKS for what the page it opened does not send back")

	assert.Equal(t, []string{installURL}, browser.urls(),
		"the address the kind named is opened, once")
	assert.Contains(t, printed.String(), "\n"+installURL+"\n",
		"and printed on a line of its own, because a launch that silently does nothing "+
			"leaves the printed address as the only way through")
}

// TestRunHandoff_AnAddressThatCannotBeOpenedIsPrintedRatherThanFatal is the
// headless degradation. A remote shell, a container, a misconfigured default
// browser: none of them are a reason to lose a run whose App already exists.
func TestRunHandoff_AnAddressThatCannotBeOpenedIsPrintedRatherThanFatal(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	const installURL = "https://github.com/apps/demo-reviewbot-gh/installations/new"
	run, st := handoffParams(t, openAfterSpec(t, svc.URL, installURL), func(u string) error {
		if strings.HasPrefix(u, "http://127.0.0.1:") {
			return fakeBrowser(t, svc.URL, nil)(u)
		}
		return errors.New("no browser on this host")
	}, "67890\n")
	var printed bytes.Buffer
	run.opts.Out = &printed

	answers, err := runHandoff(context.Background(), run)
	require.NoError(t, err, "a browser that will not open must not cost the run")
	assert.Equal(t, "67890", answers["installation-id"])

	assert.Contains(t, printed.String(), installURL,
		"the address is still handed over, which is the whole floor this degrades to")
	assert.Contains(t, strings.Join(noteValues(st), "\n"), "no browser on this host",
		"and the reason is recorded rather than dropped")
}

// TestRunHandoff_AnUnattendedRunPrintsTheAddressAndOpensNothing. Nobody is
// watching, so there is no browser to open one in front of — but the address
// is the only way the run's operator ever learns what is still owed.
func TestRunHandoff_AnUnattendedRunPrintsTheAddressAndOpensNothing(t *testing.T) {
	const installURL = "https://github.com/apps/demo-reviewbot-gh/installations/new"
	var browser browserLog

	spec := &channelkinds.HandoffSpec{
		Begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
			return channelkinds.HandoffStart{URL: "https://example.test/new"}, nil
		},
		Complete: func(context.Context, url.Values) (map[string]string, error) { return nil, nil },
		FallbackInputs: []oap.Question{
			{Name: "app-id", Type: oap.QString, Prompt: "App ID"},
			{Name: "installation-id", Type: oap.QString, Prompt: "Installation ID"},
		},
		FallbackOpenURL: func(map[string]string) string { return installURL },
	}

	run, _ := handoffParams(t, spec, browser.open, "")
	run.opts.NonInteractive = true
	run.seeded = map[string]string{"app-id": "12345"}
	var printed bytes.Buffer
	run.opts.Out = &printed

	// The run cannot finish — installation-id is unanswered and nobody is here
	// to be asked — but that is the QUESTION's refusal, not the browser's.
	_, err := runHandoff(context.Background(), run)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "browser could not be opened",
		"the address is a courtesy; it is never what fails a run")

	assert.Empty(t, browser.urls(), "an unattended run has no browser to open anything in")
	assert.Contains(t, printed.String(), installURL)
}

// noteValues is every line a run recorded on its State, which is what the
// post-run summary is rendered from.
func noteValues(st *tui.State) []string {
	notes := st.Notes()
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.Value)
	}
	return out
}
