// The CLI's driver for a kind's browser handoff (channelkinds.HandoffSpec):
// the step no form can express, where the operator is sent to an external
// service and the result comes back on a callback.
//
// THE CLIENT OWNS THE CALLBACK, which is what makes one spec drive both this
// and a server-rendered UI. Here that means a loopback listener on an
// ephemeral port, a self-submitting form when the service wants a POST, and
// the CSRF nonce — generated here, set on the address the operator is sent to,
// and required back on the callback before anything is exchanged.
//
// A handoff that does not finish is a DETOUR, not the end of the run: the
// spec's fallback questions are what the operator answers instead, and they
// are also what is asked for whatever a completed handoff did not supply (see
// HandoffSpec.FallbackInputs). A kind that declared no fallback fails
// outright, and so does one that could not describe the round trip in the
// first place — see handoffSetupError for why those two are different
// failures.
package channelwizard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// defaultHandoffTimeout bounds how long a run waits for the service to
// redirect back before giving up and asking the fallback questions instead.
// Long enough for a human to read a confirmation page and click through; short
// enough that a browser that never opened does not hang the terminal
// indefinitely. It is the same five minutes the github wizard's own screen
// path waits (defaultAppCreateTimeout), which this replaces.
const defaultHandoffTimeout = 5 * time.Minute

// handoffRun is one handoff and everything the client brings to it.
//
// One struct rather than seven parameters because they travel together and
// several are only meaningful next to another: seeded is read against the
// spec's own fallback questions, and state is the State those seeded answers
// are written into.
type handoffRun struct {
	spec *channelkinds.HandoffSpec

	// answers is what the up-front questions collected. The spec's closures
	// derive everything they need from it — the address the operator is sent
	// to, the body POSTed there, the guidance above the fallback.
	answers map[string]string
	// seeded is what --answer supplied, keyed the way a question names it, so
	// a fallback question a flag already answered is not asked again.
	seeded map[string]string
	// state is the run's State: the same one every other screen of this run is
	// driven against, so a seeded fallback answer is readable back exactly the
	// way a typed one is.
	state *tui.State
	opts  tui.Options

	openBrowser func(string) error
	// timeout bounds the wait for the callback; zero means
	// defaultHandoffTimeout.
	timeout time.Duration
}

// runHandoff drives one handoff to completion and returns the answers it
// produced — the exchange's, plus whatever the fallback questions collected.
//
// It returns an error only when the run cannot continue: a spec the client
// cannot drive at all, a kind that could not say where to send the operator
// (handoffSetupError), a fallback question nobody answered, or a failed
// handoff for a kind that declared no fallback. A handoff that merely did not
// finish is recorded on the run's State — so the summary says why the
// automated path was not used — and the questions take over.
func runHandoff(ctx context.Context, run handoffRun) (map[string]string, error) {
	if err := validateHandoffSpec(run.spec); err != nil {
		return nil, err
	}

	// Built here and merged INTO, never reassigned: a failed exchange returns
	// no map at all, and the fallback below writes its answers into this one.
	derived := map[string]string{}
	var failure error
	// A DELIBERATE STAND-DOWN IS NOT A FAILURE, so it is carried apart from
	// one all the way down: the operator chose a route with no round trip in
	// it, and telling them "the browser step did not finish" would report
	// their own answer back to them as something that went wrong.
	standDown := run.spec.Skipped(run.answers)
	switch {
	case standDown != "":
		// Asked FIRST, ahead of the unattended check: a run with nothing to
		// hand off is owed the kind's reason rather than a sentence about a
		// browser that was never going to be opened either way.
		run.note("Browser handoff", "not needed — "+standDown)
	case run.opts.NonInteractive:
		// Nobody is watching, so there is nothing for a browser to be opened
		// in front of. Refusing here rather than launching one is what keeps
		// an unattended run from parking on a page no one will ever confirm;
		// the fallback questions are what a flag can answer, and the run fails
		// closed on the ones it cannot.
		failure = errors.New("this run is unattended, so the browser step was skipped")
	default:
		exchanged, err := run.exchange(ctx)
		// A SETUP failure is not a failed round trip and must not spend the
		// detour. The fallback exists for "the browser did not come back" —
		// a headless host, a blocked port, a service that refused — and it
		// asks the kind to build its own guidance from the SAME answers that
		// just failed to produce a starting address. So the guidance fails
		// too, and the operator is left with bare prompts for a thing they
		// must now do entirely by hand, with the actual reason (a malformed
		// answer, say) recorded only as a note under a heading that says the
		// browser step did not finish. Ending the run here says what is
		// wrong, once, in the kind's own words.
		var setup handoffSetupError
		if errors.As(err, &setup) {
			return nil, setup.err
		}
		if err != nil {
			failure = err
		}
		for k, v := range exchanged {
			derived[k] = v
		}
	}
	switch {
	case failure != nil:
		run.note("Browser handoff", failure.Error())
		if len(run.spec.FallbackInputs) == 0 {
			// No manual route: this kind's handoff is mandatory, so its
			// failure is the run's.
			return nil, failure
		}
	case standDown != "":
		// Nothing was exchanged and nothing failed. The note above already
		// said why, and noteExchange over an empty map would say "completed".
	default:
		run.noteExchange(derived)
	}

	if err := run.askFallback(ctx, derived, failure, standDown); err != nil {
		return nil, err
	}
	return derived, nil
}

// validateHandoffSpec refuses a spec no client can drive, by name.
//
// The rule is wizardrun.ValidateHandoffSpec's — a malformed spec must be
// refused identically whether a terminal or a browser is driving it, and
// wizardrun.Finish runs the same check in front of every driver. Called again
// here because runHandoff is also reached directly by this package's own
// tests, and because the checks it makes (a nil Begin, a nil Complete) guard
// dereferences that happen in THIS file.
func validateHandoffSpec(spec *channelkinds.HandoffSpec) error {
	return wizardrun.ValidateHandoffSpec(spec)
}

// exchange is the round trip: a loopback listener, the operator's browser, the
// service's redirect back, and the kind's own exchange of whatever it carried.
func (r handoffRun) exchange(ctx context.Context) (map[string]string, error) {
	if r.openBrowser == nil {
		// Checked before a listener is opened rather than at the call below:
		// a nil here is a caller that forgot to wire the seam, and a run that
		// discovered it three steps in would leave a live loopback endpoint
		// behind for the length of a panic's unwinding.
		return nil, errors.New("handoff: no way to open a browser was wired")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("handoff: listen on loopback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// EVERY path from here on goes through this: the listener accepts an
	// exchange code, and one left running answers to whatever navigates to it
	// for as long as the process lives. Which of the two teardowns applies
	// depends on whether the server ever took the listener over — Shutdown
	// closes the listeners it is serving, and it is serving none until the
	// goroutine below starts.
	srv := &http.Server{}
	safehttp.HardenServer(srv)
	serving := false
	defer func() {
		if !serving {
			if err := listener.Close(); err != nil {
				r.note("Browser handoff", "the loopback listener did not close cleanly: "+err.Error())
			}
			return
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			r.note("Browser handoff", "the loopback listener did not shut down cleanly: "+err.Error())
		}
	}()

	// A COPY of the answers, not the dispatcher's live map: this hands control
	// to a kind, and one that wrote into it would change what Resolve and
	// Result later read, from a call site nothing would attribute the change
	// back to.
	start, err := r.spec.Begin(copyAnswers(r.answers), callbackURL)
	if err != nil {
		return nil, handoffSetupError{err: err}
	}
	if strings.TrimSpace(start.URL) == "" {
		return nil, handoffSetupError{err: errors.New("handoff: the kind named no address to send the operator to")}
	}

	// The nonce is the client's because the client is what verifies the
	// callback. It rides on the query string, which is where both flows this
	// has to serve round-trip it: GitHub's App-manifest endpoint echoes it
	// onto the redirect, and `state` is the OAuth authorization endpoint's own
	// spelling of the same thing.
	nonce, err := handoffNonce()
	if err != nil {
		return nil, fmt.Errorf("handoff: generate the CSRF nonce: %w", err)
	}
	beginURL, err := urlWithState(start.URL, nonce)
	if err != nil {
		return nil, err
	}

	callbacks := make(chan handoffCallback, 1)
	mux := http.NewServeMux()
	openURL := beginURL
	if len(start.FormFields) > 0 {
		// A POST body cannot be expressed as a link, so the browser is sent to
		// a page HERE that submits one on its behalf.
		page, err := formPage(beginURL, start.FormFields)
		if err != nil {
			return nil, err
		}
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(page))
		})
		openURL = fmt.Sprintf("http://127.0.0.1:%d/", port)
	}
	mux.HandleFunc("/callback", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// CHECKED BEFORE ANYTHING IS READ OUT OF THE CALLBACK, let alone
		// exchanged: a code delivered by a page that does not know this run's
		// nonce is not this run's code, and spending it is the whole thing
		// this compare exists to stop.
		// Constant-time, because this is a secret compared against a value an
		// attacker supplies and can retry: `!=` on strings returns at the first
		// differing byte, which leaks how much of the nonce a guess got right.
		// Free here, and the kind of thing that is only ever cheap to do.
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(nonce)) != 1 {
			// Delivered as a failure rather than ignored so the run takes its
			// detour NOW. Waiting out the full timeout for a second callback
			// that is not coming would leave the operator staring at a
			// terminal for five minutes after their browser already said the
			// step went wrong.
			deliverCallback(callbacks, handoffCallback{
				err: errors.New("the callback did not match this run (CSRF protection triggered on the state parameter)"),
			})
			_, _ = w.Write([]byte(callbackMismatchPage))
			return
		}
		deliverCallback(callbacks, handoffCallback{values: q})
		_, _ = w.Write([]byte(callbackDonePage))
	})
	srv.Handler = mux
	serving = true
	go func() {
		// ErrServerClosed is this function's own deferred Shutdown and means
		// the run finished. ANYTHING else means the endpoint the browser is
		// being sent to never came up, or died under it — and the callback it
		// was going to receive is now never coming, so this ends the wait
		// rather than leaving the operator to sit out the full timeout and be
		// told the browser did not come back. That points at the browser; the
		// listener was the thing that was not there.
		//
		// Delivered on the callback channel and NOT noted from here: tui.State
		// takes no lock, and the run's own goroutine is writing notes to it.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			deliverCallback(callbacks, handoffCallback{
				err: fmt.Errorf("the loopback listener stopped serving: %w", err),
			})
		}
	}()

	// TWO ADDRESSES, AND THEY ARE NOT INTERCHANGEABLE — which is the whole
	// reason they are computed apart here and printed apart below.
	//
	// retry is an address the operator can open THEMSELVES and have this step
	// actually work. Only the loopback page qualifies: it re-serves the
	// self-submitting form, so opening it again re-POSTs everything this run
	// prepared, and it carries no nonce. It is empty when there is no such
	// address, which is the direct-navigation shape — beginURL is the only
	// thing that would work there and beginURL carries this run's CSRF nonce,
	// the one secret whose job is to be unguessable to whoever delivers the
	// callback. A terminal's scrollback outlives the run, so it is never
	// printed and the operator is told so rather than handed a lie. admind's
	// begin log makes the same choice for the same reason (see its
	// "destination" field).
	//
	// destination is where the browser ENDS UP, always start.URL, never a
	// retry target. For github's manifest flow it is a POST endpoint: the App
	// definition travels in the form body, so navigating there by hand is a
	// GET and yields a blank form every single time. Printed because knowing
	// where you are being sent is worth having — labelled, below, as exactly
	// that and not as something to click.
	retry := ""
	if openURL != beginURL {
		retry = openURL
	}
	r.explain(start.Explain, retry, start.URL)
	if err := r.openBrowser(openURL); err != nil {
		return nil, fmt.Errorf("open browser: %w", err)
	}

	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultHandoffTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case cb := <-callbacks:
		if cb.err != nil {
			return nil, cb.err
		}
		return r.spec.Complete(ctx, cb.values)
	case <-waitCtx.Done():
		return nil, fmt.Errorf("handoff: timed out waiting for the browser to come back (%v)", timeout)
	}
}

// handoffSetupError marks a failure to SET THE HANDOFF UP — the kind could not
// say where to send the operator, given the answers it was handed — as opposed
// to a round trip that started and did not come back.
//
// The distinction is what decides whether the fallback route runs. A round
// trip that failed is exactly what the fallback is for. A setup failure is
// not: the kind builds its fallback guidance from the same answers, so
// entering the detour asks it to render instructions it has just proved it
// cannot, and the operator ends up at bare prompts with the real reason
// demoted to a note under "the browser step did not finish". github's is the
// live case — a mistyped organization login fails validation inside Begin.
//
// It carries the kind's error verbatim (Error and Unwrap both defer to it), so
// nothing between the kind and the operator rewords the one sentence that says
// what to fix.
type handoffSetupError struct{ err error }

func (e handoffSetupError) Error() string { return e.err.Error() }
func (e handoffSetupError) Unwrap() error { return e.err }

// handoffCallback is what one request to the loopback endpoint amounted to:
// the values the service sent back, or why they were refused.
type handoffCallback struct {
	values url.Values
	err    error
}

// deliverCallback hands the first callback to the waiting select without ever
// blocking.
//
// The channel is buffered to exactly one, so a DUPLICATE delivery — a page
// refresh, a second tab, a browser prefetching the URL — would otherwise wedge
// its handler goroutine forever waiting for a receiver that already left, and
// that in turn stalls the deferred shutdown. Dropping the duplicate is
// harmless: only the first result was ever going to be used.
func deliverCallback(callbacks chan handoffCallback, cb handoffCallback) {
	select {
	case callbacks <- cb:
	default:
	}
}

// askFallback asks for whatever the handoff did not supply.
//
// failure is nil when the round trip completed, in which case the questions
// left are the ones it was never going to answer — github's installation ID,
// which GitHub does not redirect back with. It is non-nil when the handoff did
// not happen, and then it is also the first thing the operator reads: being
// asked to create something by hand needs the sentence explaining why.
//
// standDown is the kind's own reason for there having been no round trip to
// make (HandoffSpec.SkipWhen). It reads where failure would, and is carried
// apart from it because it is not one: the operator chose this route.
func (r handoffRun) askFallback(ctx context.Context, derived map[string]string, failure error, standDown string) error {
	// Asked of the spec rather than filtered here: which questions an exchange
	// leaves outstanding is a semantic of the contract — a question the
	// handoff answers under another name is still answered — and a client that
	// decided it for itself would ask a different set than the next one.
	remaining := r.spec.Unanswered(derived)
	if len(remaining) == 0 {
		return nil
	}
	r.prependGuidance(remaining, derived, failure, standDown)
	// AFTER the guidance is built and BEFORE the screen is rendered. The
	// screen takes the terminal over, and the answer to the questions on it is
	// on the page being opened.
	r.openBeforeFallback(derived)

	screens, err := renderQuestions(remaining, r.seeded, r.state, screenCaps(r.opts))
	if err != nil {
		return err
	}
	byScreen := declaredAnswerKeys(screens)
	answered, err := driveChannelScreens(ctx, screens, r.state, byScreen, r.opts)
	if err != nil {
		return err
	}
	for k, v := range answersFrom(remaining, answered) {
		derived[k] = v
	}
	return nil
}

// prependGuidance puts the reason and the kind's own instructions above the
// FIRST question that will actually be rendered.
//
// On the first question and not each of them because that is where the screen
// contract put it: appManualScreen shows one huh note above its four fields.
// And on the first RENDERED one because a question a flag already answered is
// dropped before a screen is built for it, so guidance attached there would be
// dropped with it.
//
// It is carried as the question's Description, which tui.Question renders as a
// note title above the field — visible under the line-oriented driver too,
// where a huh Description is not.
func (r handoffRun) prependGuidance(remaining []oap.Question, derived map[string]string, failure error, standDown string) {
	// WHICH question gets the guidance is settled BEFORE the guidance is
	// built, because a run with nothing left to render must not build it at
	// all: HandoffSpec.FallbackGuidance promises it is called only when a
	// question is actually going to be asked, and a kind may do real work
	// inside it — github writes its reference manifest to disk from there.
	// Every remaining question already answered by a flag means there is no
	// screen to carry the text and no operator to read it.
	first := -1
	for i := range remaining {
		if _, seeded := r.seeded[remaining[i].Name]; !seeded {
			first = i
			break
		}
	}
	if first < 0 {
		return
	}

	var blocks []string
	switch {
	case standDown != "":
		// The kind's own sentence, and no talk of a browser step: this run was
		// never going to make the round trip, because the operator said it had
		// nothing to do.
		blocks = append(blocks, standDown)
	case failure != nil:
		blocks = append(blocks, "The browser step did not finish ("+failure.Error()+"),\n"+
			"so this asks for what it would have supplied.")
	}
	if r.spec.FallbackGuidance != nil {
		guidance, err := r.spec.FallbackGuidance(r.knownAnswers(derived))
		switch {
		case err != nil:
			// Not fatal: the questions are the floor that always works, and an
			// operator who knows the values must not lose them because the
			// guidance could not be built.
			//
			// But not merely NOTED either. A note lands in the post-run
			// summary, which is printed after the run ends — long after these
			// questions have been put to someone who was relying on the
			// guidance to answer them. github's fallback carries the webhook
			// URL, the permissions and the events for an App the operator is
			// about to create by hand: without it they face five bare prompts
			// and no way to know that anything is missing. So the failure is
			// stated where the questions are, as well as recorded.
			r.note("Browser handoff", "the guidance for these questions could not be rendered: "+err.Error())
			blocks = append(blocks, "The instructions that belong above these questions\n"+
				"could not be built ("+err.Error()+"),\n"+
				"so they are missing here.")
		case strings.TrimSpace(guidance) != "":
			blocks = append(blocks, guidance)
		}
	}
	if len(blocks) == 0 {
		return
	}
	if d := strings.TrimSpace(remaining[first].Description); d != "" {
		blocks = append(blocks, d)
	}
	remaining[first].Description = strings.Join(blocks, "\n\n")
}

// copyAnswers is the answer map as a value a kind may do anything with.
func copyAnswers(answers map[string]string) map[string]string {
	out := make(map[string]string, len(answers))
	for k, v := range answers {
		out[k] = v
	}
	return out
}

// knownAnswers is every answer in hand by the time the fallback is asked: the
// up-front questions', whatever a flag supplied for a fallback question, and
// whatever the handoff itself produced — the last of those winning, because it
// came from the service.
func (r handoffRun) knownAnswers(derived map[string]string) map[string]string {
	known := make(map[string]string, len(r.answers)+len(derived))
	for k, v := range r.answers {
		known[k] = v
	}
	for _, q := range r.spec.FallbackInputs {
		if v, ok := r.seeded[q.Name]; ok {
			known[q.Name] = v
		}
	}
	for k, v := range derived {
		known[k] = v
	}
	return known
}

// explain tells the operator what is about to open, which address they can
// re-open if it goes wrong, and where the browser ends up.
//
// Written between two runs of the sequencer, where the alternate screen is
// down, so it lands in the ordinary buffer and stays in scrollback. That is
// the only reason any of it is useful: the case it exists for is a launch that
// silently does nothing — a misconfigured default browser, a remote shell, a
// signed-out session — which the operator can only recover from by opening
// something themselves.
//
// WHICH ADDRESS IS ACTIONABLE IS THE WHOLE POINT, and getting it wrong is what
// this wording replaced. The two URLs read alike and behave nothing alike: the
// loopback page re-submits everything this run prepared, while the service
// address is a POST endpoint that a hand-typed GET reaches empty. Printing
// both under one neutral "which takes you to" invited the operator to click
// the one that can only ever fail, so each now says what it is FOR.
//
// retry is empty when this run has no address it can honestly offer — the
// direct-navigation shape, where the only working address carries the CSRF
// nonce. Saying so is better than either printing the nonce or printing a
// nonce-stripped copy that would fail the callback's constant-time compare and
// send the run down its fallback for a reason the operator caused by following
// this very text.
//
// EACH URL GETS A LINE TO ITSELF, unwrapped and unelided. Nothing here is
// width-budgeted — this writes straight to the run's Out, outside the chrome
// that sizes the form body — but the rule would hold anyway: a URL broken
// across lines or trimmed with an ellipsis stops being a thing a terminal can
// click or a reader can retype, which is the entire reason it is printed.
func (r handoffRun) explain(what, retry, destination string) {
	if r.opts.Out == nil {
		return
	}
	out := r.opts.Out
	if what = strings.TrimSpace(what); what != "" {
		fmt.Fprintf(out, "\n%s\n", what)
	}
	retry, destination = strings.TrimSpace(retry), strings.TrimSpace(destination)

	if retry == "" {
		// Nothing to hand over. Name where the browser is going, and say
		// plainly that this step is not one the operator can restart by hand,
		// so a launch that failed is waited out rather than fought.
		fmt.Fprintf(out, "\nOpening your browser, which takes you to:\n%s\n", destination)
		fmt.Fprintln(out, "This step cannot be re-opened by hand: the address your browser is sent to carries")
		fmt.Fprintln(out, "a one-time value this run must keep to itself. If the browser does not open, wait —")
		fmt.Fprintln(out, "this run falls back to asking you for the details instead.")
		return
	}

	fmt.Fprintf(out, "\nOpening your browser. IF IT DOES NOT OPEN, OR THIS STEP GOES WRONG, open this\naddress yourself — it re-sends everything this run prepared:\n%s\n", retry)
	if destination != "" {
		fmt.Fprintf(out, "\nThat page takes you on to:\n%s\n", destination)
		fmt.Fprintln(out, "Shown so you know where you are being sent. Going there directly is NOT a retry —")
		fmt.Fprintln(out, "it carries none of this run's details.")
	}
}

// openBeforeFallback puts the address the kind named
// (HandoffSpec.FallbackOpenURL) in front of the operator: github's App
// installation page, once the App exists.
//
// FIRE AND FORGET, AND NEVER FATAL. There is no callback and nothing to wait
// for — GitHub redirects an installation back only to an App that declares a
// Setup URL, and this one does not — so the questions below are asked exactly
// as they would have been. A browser that cannot be launched, or a run with
// nobody to launch one for, degrades to the printed address and carries on:
// losing a run over a convenience would be far worse than the copy and paste
// it saves.
//
// THE ADDRESS IS PRINTED WHATEVER HAPPENS, on a line of its own, for the
// reason explain's is: this writes to the run's Out, outside the alt-screen,
// so it survives in scrollback — and a launch that silently opens the wrong
// browser profile or nothing at all leaves the printed line as the only way
// through. It is not width-budgeted and must never be wrapped or elided; a cut
// URL is no longer something a terminal can linkify or a reader can retype.
func (r handoffRun) openBeforeFallback(derived map[string]string) {
	target := r.spec.FallbackOpen(r.knownAnswers(derived))
	if target == "" {
		return
	}
	// Nobody is watching, or nothing was wired to open one. Either way the
	// address is all this can offer, and offering it is strictly better than
	// dropping it: it is the page the next question's answer is read off.
	if r.opts.NonInteractive || r.openBrowser == nil {
		r.printAddress("Open this to continue:", target, "")
		return
	}
	if err := r.openBrowser(target); err != nil {
		// Recorded as well as printed. The note reaches the post-run summary,
		// which is where an operator reconstructing what happened looks; the
		// printed line is what they need NOW, while the questions are in front
		// of them.
		r.note("Browser handoff", "could not open "+target+": "+err.Error())
		r.printAddress("Your browser could not be opened ("+err.Error()+"). Open this yourself:", target, "")
		return
	}
	r.printAddress("Opening your browser at:", target,
		"This run does NOT wait for that page — it never sends anything back here. "+
			"Answer below\nonce you have what it asks for.")
}

// printAddress writes one address to the run's Out with a lead line above and
// an optional note below, keeping the URL alone on its line.
func (r handoffRun) printAddress(lead, url, after string) {
	if r.opts.Out == nil {
		return
	}
	fmt.Fprintf(r.opts.Out, "\n%s\n%s\n", lead, url)
	if after != "" {
		fmt.Fprintln(r.opts.Out, after)
	}
}

// noteExchange records what the round trip produced, ON THE RUN'S STATE and
// not only in the answer map.
//
// A handoff is the one step of a channel-create run that leaves something
// behind in the WORLD: github's creates a real GitHub App whose private key
// GitHub will never show again. If the run then fails — the operator cancels
// at the next question, the apply is refused — Result is never reached, so
// WizardOutput.Summary never exists, and the answer map goes out of scope with
// it. What the State holds is all ReportUnfinished has left to print, so
// this line is recorded the moment the exchange returns rather than left for
// a Result that may never run.
//
// A VALUE IS ONLY EVER PRINTED FOR A KEY THE KIND DECLARED AS NON-SECRET.
// A SummaryNote reaches plain scrollback verbatim, an exchange routinely
// returns credentials alongside identifiers (github's returns the App's
// private key and webhook secret next to its id and slug), and this client
// cannot tell them apart on its own — so a QSecret question's answer, and any
// key no fallback question declares at all, contributes its NAME and nothing
// more. Unknown means secret here, which is the only safe direction.
func (r handoffRun) noteExchange(derived map[string]string) {
	if len(derived) == 0 {
		return
	}
	printable := make(map[string]bool, len(r.spec.FallbackInputs))
	for _, q := range r.spec.FallbackInputs {
		printable[q.Name] = q.Type != oap.QSecret
	}
	parts := make([]string, 0, len(derived))
	// Ordered by the kind's own declaration, then by whatever it returned that
	// no question declares, so the line is stable across runs.
	for _, q := range r.spec.FallbackInputs {
		if v, ok := derived[q.Name]; ok {
			parts = append(parts, describeExchanged(q.Name, v, printable[q.Name]))
		}
	}
	undeclared := make([]string, 0, len(derived))
	for k := range derived {
		if _, declared := printable[k]; !declared {
			undeclared = append(undeclared, k)
		}
	}
	sort.Strings(undeclared)
	for _, k := range undeclared {
		parts = append(parts, describeExchanged(k, derived[k], false))
	}
	r.note("Browser handoff", "completed — "+strings.Join(parts, ", "))
}

// describeExchanged renders one thing the exchange produced: "name=value" when
// the kind declared it as something an operator may read back, and the bare
// name otherwise.
func describeExchanged(name, value string, printable bool) string {
	if printable {
		return name + "=" + value
	}
	return name
}

// note records a line on the run's State, which is what the post-run summary
// is rendered from.
//
// A nil State is not a shape production produces — the dispatcher always
// drives a handoff against the run's own State — but a note is a record, not a
// step, and it must not panic a run that got this far.
func (r handoffRun) note(label, value string) {
	if r.state == nil {
		return
	}
	r.state.Note(label, value)
}

// handoffNonce returns the CSRF nonce for one handoff: round-tripped through
// the service and required back on the callback.
func handoffNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// urlWithState puts the nonce on the address the operator is sent to, keeping
// whatever query the kind already put there.
func urlWithState(raw, nonce string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("handoff: the kind's address %q is not a URL: %w", raw, err)
	}
	q := u.Query()
	q.Set("state", nonce)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// formTemplate renders the self-submitting page through html/template rather
// than fmt.Sprintf, so the action URL and every field value are escaped for
// the attribute context they land in by the library that knows the rule —
// these are values a kind composed, and getting HTML-attribute escaping right
// by hand is exactly the kind of detail that looks fine until it isn't.
//
// The fields are ranged over a map, which text/template walks in sorted key
// order, so the same spec renders the same page every time.
var formTemplate = template.Must(template.New("handoff-form").Parse(`<!doctype html>
<html>
<head><meta charset="utf-8"><title>Continuing…</title></head>
<body onload="document.forms[0].submit()">
<p>Redirecting you to finish this step. This needs JavaScript enabled and a
browser that can reach the service.</p>
<form method="post" action="{{.Action}}">
{{range $name, $value := .Fields}}<input type="hidden" name="{{$name}}" value="{{$value}}">
{{end}}<noscript><button type="submit">Continue</button></noscript>
</form>
</body>
</html>
`))

func formPage(action string, fields map[string]string) (string, error) {
	var b strings.Builder
	err := formTemplate.Execute(&b, struct {
		Action string
		Fields map[string]string
	}{Action: action, Fields: fields})
	if err != nil {
		return "", fmt.Errorf("handoff: render the submit page: %w", err)
	}
	return b.String(), nil
}

// The two pages the operator's browser lands on when the service sends them
// back. Neither says anything about what the exchange produced: the exchange
// runs after the page is written, and the terminal is where its outcome
// belongs.
//
// Written out as constants rather than rendered from a template with the
// message interpolated: nothing here varies with anything the run collected,
// so there is no value to escape, and a template with no data would only add
// an Execute error on a path that has nowhere useful to report one.
const (
	callbackDonePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Back to your terminal</title></head>
<body><p>Done. You can close this tab and return to your terminal.</p></body></html>
`

	callbackMismatchPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Back to your terminal</title></head>
<body><p>This browser session does not match the run that started it. Close
this tab and return to your terminal.</p></body></html>
`
)
