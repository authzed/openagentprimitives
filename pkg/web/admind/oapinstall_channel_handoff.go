// The step no form can express: sending the operator to an external service in
// a browser, and receiving the callback it sends them back with.
//
// THE CLIENT OWNS THE CALLBACK, which is what lets one channelkinds.HandoffSpec
// drive both `oap channel create` and this. For the CLI that means a loopback
// listener on an ephemeral port and a single blocking call. HERE it is two
// requests: a POST that returns where to send the browser, and a GET the
// service redirects into. Everything that must survive between them — the live
// spec, the answers Begin was called with, and this run's CSRF nonce — is on
// the pending setup, which is why that store exists in the shape it does.
//
// A HANDOFF THAT DOES NOT FINISH IS A DETOUR, not the end of the run: the
// spec's fallback questions are what the operator answers instead, and they are
// also what is asked for whatever a completed exchange did not supply (github's
// installation ID is asked on every route, because GitHub redirects an App's
// creation back and an installation not). Which questions survive is
// HandoffSpec.Unanswered's answer and not this file's.
//
// A BEGIN THAT FAILS ENDS THE RUN. It is not a failed round trip and must not
// spend the detour: Begin and FallbackGuidance are built by the same kind from
// the same answers, so a Begin that could not describe where to send the
// operator is a FallbackGuidance that cannot describe the manual route either
// — and entering the detour buys them bare prompts plus a note saying the
// browser step did not finish, with the real reason demoted to a footnote. The
// CLI shipped that bug once; see channelkinds.HandoffSpec.Begin.
package admind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

const (
	// channelHandoffPath begins a round trip. POST, because it mints a nonce
	// and can create a real App at the far end.
	channelHandoffPath = "/admin/v1/agents/channel-handoff"
	// channelHandoffCallbackPath is where the external service redirects the
	// operator's browser back to. GET, because that is what a redirect is.
	channelHandoffCallbackPath = channelHandoffPath + "/callback"

	// APIPathPrefix is the path prefix admind's own mux serves under, and the
	// prefix pkg/web/adminui's reverse proxy rewrites TO.
	APIPathPrefix = "/admin/v1/"
	// BrowserAPIPathPrefix is the prefix a BROWSER reaches those same routes
	// under: adminui proxies /admin/api/<rest> to <admind>/admin/v1/<rest>,
	// attaching the service token and the authenticated subject server-side.
	//
	// BOTH ARE EXPORTED AND adminui READS THEM, which is the point. The
	// direction is deliberate — admind cannot import adminui, because doing so
	// would run its init() and register webd's admin UI into the webui registry
	// as a side effect of linking admind, which the operator does not host — so
	// the constants live on the side that can be imported and adminui's
	// proxyHandler builds its rewrite out of them. That leaves the pattern
	// strings in adminui's own route table as the only remaining spellings, and
	// TestEveryProxyPatternUsesTheSharedPrefix pins those against this constant.
	//
	// An earlier version of this comment claimed a test pinned the two halves
	// together; it did not — it composed admind's own two constants and never
	// mentioned adminui, which is the transcribed-list defect wearing the
	// clothes of a guard.
	BrowserAPIPathPrefix = "/admin/api/"
)

// channelHandoffRequest begins a round trip for one declared channel.
type channelHandoffRequest struct {
	// SetupToken is the same handle the submit route takes; the handoff and
	// the answers that follow it are one setup.
	SetupToken string `json:"setupToken"`
	// Answers is what the operator filled in BEFORE the browser leaves — the
	// kind's own up-front questions. github's organization is the live case:
	// it goes into the address the browser is sent to and into the manifest
	// POSTed there, so the round trip cannot be described without it.
	Answers map[string]string `json:"answers"`
}

// channelHandoffResponse is where to send the browser.
//
// It carries nothing secret: Begin runs before any credential exists, and the
// exchange that mints them happens server-side on the callback.
type channelHandoffResponse struct {
	// SetupToken is echoed so the UI can submit the fallback answers against
	// the same setup without holding it separately.
	SetupToken string `json:"setupToken"`
	// NextAction lets a kind decline its browser step and direct every client
	// back through ordinary setup without exposing provider-specific logic.
	NextAction string `json:"nextAction,omitempty"`
	// Explain is what the kind wants the operator to know before the browser
	// goes — what is about to be created, and where.
	Explain string `json:"explain,omitempty"`
	// URL is the address to send the browser to, with this run's `state` on
	// its query.
	URL string `json:"url"`
	// FormFields, when present, must be POSTed to URL as an HTML form rather
	// than navigated to. GitHub's App-manifest flow needs a POST body carrying
	// the manifest JSON; a GET cannot express it. The UI renders a
	// self-submitting form, exactly as the CLI serves one from its loopback
	// listener.
	FormFields map[string]string `json:"formFields,omitempty"`
}

// channelHandoffCallbackResponse is what the operator's browser lands on after
// the external service sends them back.
//
// IT CARRIES NOTHING THE EXCHANGE PRODUCED. Complete runs server-side
// precisely so the App's private key and webhook secret never enter a browser;
// putting any of it here — even to show the operator what was created — would
// undo the one property that placement buys.
type channelHandoffCallbackResponse struct {
	SetupToken string `json:"setupToken"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Kind       string `json:"kind"`
	// Exchanged reports that the round trip completed and its answers are held
	// server-side against this setup.
	Exchanged bool `json:"exchanged"`
	// Questions is what the operator still owes an answer to — the fallback
	// questions the exchange did not satisfy. Empty means the next POST to the
	// setup route needs no answers at all.
	Questions []oap.Question `json:"questions,omitempty"`
	// Guidance is the kind's own instructions above those questions, built
	// from every answer known by now. Empty when the kind offers none.
	Guidance string `json:"guidance,omitempty"`
	// GuidanceError says the instructions could not be built, and is not a
	// failure of the run: the questions are the floor that always works, and
	// an operator who knows the values must not lose them because the prose
	// could not be rendered. Reported rather than swallowed.
	GuidanceError string `json:"guidanceError,omitempty"`
}

// handleChannelHandoffBegin describes the round trip and hands the browser its
// starting address.
//
// THE ORDER MATTERS AND MIRRORS wizardrun.Finish's. The collision check comes
// FIRST, because Begin is where the irreversible half happens: github's creates
// a real GitHub App, and an operator sent to create one for a Channel name that
// is already taken loses the run AND is left with an App whose private key was
// minted for a Channel that will never exist (P5-R21).
func (a *Admind) handleChannelHandoffBegin(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	owner, ok := a.channelSetupOwner(w, r)
	if !ok {
		return
	}

	var req channelHandoffRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChannelSetupBody)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}

	rec, err := a.channelSetups.get(req.SetupToken, owner)
	if err != nil {
		a.cfg.Logger.Info("admind: channel handoff for an unknown, expired, or unowned token",
			"subject", owner.String(), "err", err.Error())
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	if refuseCompletedGraphSetup(w, &rec) {
		return
	}
	rec.redactor.register(req.Answers)
	nonce := ""
	graphHandoffClaimed := false
	if rec.graphBinding != nil {
		nonce, err = newSetupToken()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not start the browser step; see operator logs")
			return
		}
		rec, err = a.channelSetups.beginGraphHandoff(rec.token, owner, nonce)
		if err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		graphHandoffClaimed = true
		defer func() {
			if !graphHandoffClaimed {
				return
			}
			if cleanupErr := a.channelSetups.failGraphHandoff(context.WithoutCancel(r.Context()), rec.token, rec.owner, nonce, graphSetupHandoffBeginning); cleanupErr != nil {
				a.cfg.Logger.Info("admind: failed graph handoff cleanup", "namespace", rec.namespace, "channel", rec.required.Name, "err", cleanupErr.Error())
			}
		}()
	}

	prep, ok := a.prepareChannelSetup(w, r, &rec)
	if !ok {
		return
	}
	if prep.alreadyWired {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf(
			"Channel %q is already wired to %q; there is nothing to set up",
			rec.required.Name, rec.agentClass))
		return
	}
	if prep.handoff == nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf(
			"setting up a %s channel needs no browser round trip; POST the answers to %s instead",
			rec.required.Kind, channelSetupPath))
		return
	}
	// Validated before a nonce is minted or an address is built: Begin is
	// dereferenced two lines below, and a spec whose fallback questions no
	// renderer can honor would otherwise only be discovered after the App
	// exists.
	if err := wizardrun.ValidateHandoffSpec(prep.handoff); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	answers, err := mergeChannelAnswers(
		wizardrun.AllInputs(prep.inputs, prep.handoff), rec.seeded, nil, req.Answers)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, rec.redactor.string(err.Error()))
		return
	}
	rec.redactor.register(answers)
	// Only the UP-FRONT questions are owed an answer here. The fallback ones
	// are what the round trip is FOR — asking for them before it runs would
	// demand the App's private key from an operator who has not created the
	// App yet.
	if missing := unansweredChannelQuestions(prep.inputs, rec.seeded, answers); len(missing) > 0 {
		if rec.graphBinding != nil {
			if err := a.channelSetups.releaseGraphHandoffBegin(rec.token, owner, nonce); err != nil {
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			}
			graphHandoffClaimed = false
		}
		writeJSON(w, http.StatusBadRequest, oapInstallMissingQuestionsResponse{
			Error:     "missing required answer(s) before the browser step for channel " + rec.required.Name,
			Questions: missing,
		})
		return
	}

	// THE KIND MAY DECLINE THE ROUND TRIP FROM THE ANSWERS, and it is asked
	// before a nonce is minted or an App can be created. github's route
	// question is the live case: an operator who says they already have a
	// GitHub App must not be sent to register a second one, and the answer
	// that settles that only exists here — Wizard.Handoff is described before
	// any question is answered, so the spec could not decline itself.
	//
	// Refused the same way a kind with no handoff at all is, because it is the
	// same answer for this run: there is nothing to begin, and the setup route
	// is where the remaining questions land. Asking the SPEC rather than
	// deciding here is what keeps this and `oap channel create` from sending
	// different operators to different places off identical answers.
	if why := prep.handoff.Skipped(answers); why != "" {
		if rec.graphBinding != nil {
			if err := a.channelSetups.releaseGraphHandoffBegin(rec.token, owner, nonce); err != nil {
				writeJSONError(w, http.StatusConflict, err.Error())
				return
			}
			graphHandoffClaimed = false
		}
		writeJSON(w, http.StatusOK, channelHandoffResponse{
			SetupToken: rec.token,
			NextAction: "setup",
			Explain:    rec.redactor.string(why),
		})
		return
	}

	// In FRONT of Begin. See this function's doc.
	if err := wizardrun.RefuseExisting(r.Context(), a.cfg.K8s, rec.namespace, answers[wizardkeys.KeyChannelName]); err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}

	callbackURL, err := a.channelHandoffCallbackURL(&rec)
	if err != nil {
		// Fail closed rather than send the operator to create an App that
		// redirects nowhere: GitHub reads the callback out of the manifest it
		// is POSTed, so an empty one produces an App and no way back.
		a.cfg.Logger.Info("admind: cannot begin a channel handoff without a browser-reachable callback",
			"namespace", rec.namespace, "channel", rec.required.Name, "err", err.Error())
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if nonce == "" {
		nonce, err = newSetupToken()
		if err != nil {
			a.cfg.Logger.Info("admind: could not generate a handoff CSRF nonce",
				"namespace", rec.namespace, "channel", rec.required.Name, "err", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "could not start the browser step; see operator logs")
			return
		}
	}

	// A COPY of the answers, not the map this handler goes on to store: this
	// hands control to a kind, and one that wrote into it would change what
	// Resolve and Result later read from a call site nothing would attribute
	// the change back to.
	start, err := prep.handoff.Begin(copyAnswers(answers), callbackURL)
	if err != nil {
		safeErr := rec.redactor.string(err.Error())
		// FATAL, and deliberately not a fallback. See the package doc.
		a.cfg.Logger.Info("admind: the kind could not describe its browser step",
			"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "err", safeErr)
		writeJSONError(w, http.StatusBadRequest, safeErr)
		return
	}
	if strings.TrimSpace(start.URL) == "" {
		writeJSONError(w, http.StatusBadRequest, "handoff: the kind named no address to send the operator to")
		return
	}
	beginURL, err := urlWithHandoffState(start.URL, nonce)
	if err != nil {
		safeErr := rec.redactor.string(err.Error())
		a.cfg.Logger.Info("admind: the kind returned an invalid browser-step address",
			"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "err", safeErr)
		writeJSONError(w, http.StatusBadRequest, safeErr)
		return
	}

	// Stored only once every step that could still refuse has passed, so a
	// refused begin leaves no nonce a callback could match — and stored THROUGH
	// the store, under its lock, because rec is this request's own copy and a
	// callback arriving on another goroutine reads the record itself.
	//
	// handoffDerived is cleared: a second begin supersedes whatever an earlier
	// round trip exchanged, and leaving it would let the next submit finish
	// against a spec this nonce no longer belongs to.
	if rec.graphBinding != nil {
		err = a.channelSetups.completeGraphHandoffBegin(rec.token, owner, nonce, prep.handoff, prep.in, answers)
		if err == nil {
			graphHandoffClaimed = false
		}
	} else {
		_, err = a.channelSetups.update(rec.token, owner, func(p *pendingChannelSetup) {
			p.handoff = prep.handoff
			p.handoffIn = prep.in
			p.handoffAnswers = maps.Clone(answers)
			p.handoffState = nonce
			p.handoffDerived = nil
		})
	}
	if err != nil {
		a.cfg.Logger.Info("admind: a channel handoff was described but its setup was gone before it could be stored",
			"namespace", rec.namespace, "channel", rec.required.Name, "err", err.Error())
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}

	// LOGGED BEFORE THE OPERATOR IS REDIRECTED, and naming what is about to be
	// created, because this line is the only durable record that it was.
	//
	// The next thing that happens is a real App on somebody's organization, and
	// the pending setup that could match its callback lives only in this
	// process's memory. A restart between here and the callback orphans that
	// App — and no API lists a user's apps afterwards, on GitHub or on Slack —
	// so an operator with nothing but the log has to be able to tell what was
	// created and where.
	//
	// WHAT NAMES IT is the kind's own Explain and its own destination address,
	// not a field this package reads out of the answers. admind does not know
	// what an "org" is and must not learn: the address github sends the
	// operator to embeds the organization, and Explain names the App. Reading
	// answers["org"] here would be the `if kind == "github"` this repo refuses,
	// spelled as a map key.
	//
	// THE NONCE IS NOT LOGGED. It is the secret whose whole job is to be
	// unguessable to whoever delivers the callback, so start.URL is logged and
	// beginURL — which carries it — is not. The setup TOKEN is logged: it is
	// not a bearer credential (the platform permission and the owner check both
	// still gate every use of it), and it is what correlates this line with the
	// exchange and creation lines that follow.
	a.cfg.Logger.Info("admind: channel handoff begun — an external resource is about to be created",
		"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind,
		"subject", owner.String(), "setupToken", rec.token,
		"destination", rec.redactor.string(start.URL), "explain", rec.redactor.string(start.Explain), "callback", callbackURL)
	writeJSON(w, http.StatusOK, channelHandoffResponse{
		SetupToken: rec.token,
		Explain:    rec.redactor.string(start.Explain),
		URL:        beginURL,
		FormFields: start.FormFields,
	})
}

// handleChannelHandoffCallback receives the redirect the external service sends
// the operator back with, and exchanges what it carries — server-side.
//
// THE STATE IS VERIFIED BEFORE THE CODE IS READ, and the ordering is the point
// rather than a preference. A code delivered by a page that does not know this
// run's nonce is not this run's code, and spending it is exactly what the
// verification exists to stop. So this handler reads `state` and NOTHING else
// off the query until a matching pending setup has been claimed; only then are
// the callback's values materialized and handed to Complete.
func (a *Admind) handleChannelHandoffCallback(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.channelSetupOwner(w, r)
	if !ok {
		return
	}

	// The ONLY thing read off the callback before it is verified.
	//
	// CLAIMED, not looked up: matching the nonce and spending it are one
	// operation inside the store, under one lock. Two deliveries of the same
	// redirect — a browser prefetch and the navigation the operator actually
	// made — would otherwise both match before either cleared, and both would
	// go on to spend the code at the external service.
	state := r.URL.Query().Get("state")
	rec, err := a.channelSetups.claimHandoffState(state, owner)
	if err != nil {
		a.cfg.Logger.Info("admind: a channel-handoff callback did not match any run this operator started",
			"subject", owner.String())
		writeJSONError(w, http.StatusBadRequest,
			"this callback does not match a browser step you started (CSRF protection); start the channel setup again")
		return
	}

	// Verified. The rest of the callback may now be read.
	callback := r.URL.Query()
	callbackAnswers := make(map[string]string, len(callback))
	for key, values := range callback {
		callbackAnswers[key] = strings.Join(values, ",")
	}
	rec.redactor.register(callbackAnswers)
	derived, err := rec.handoff.Complete(r.Context(), callback)
	rec.redactor.register(derived)
	if err != nil {
		safeErr := rec.redactor.string(err.Error())
		if rec.graphBinding != nil {
			cleanupErr := a.channelSetups.failGraphHandoff(context.WithoutCancel(r.Context()), rec.token, owner, rec.handoffState, graphSetupHandoffCompleting)
			if cleanupErr != nil {
				safeErr = rec.redactor.string(errors.Join(errors.New(safeErr), cleanupErr).Error())
			}
		}
		a.cfg.Logger.Info("admind: a channel handoff came back but its exchange failed",
			"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "err", safeErr)
		writeJSONError(w, http.StatusBadRequest, safeErr)
		return
	}
	// Written back through the store, which owns the record: rec is this
	// request's own copy, and merging into it alone would lose the exchange the
	// moment the operator submits the fallback answers. The merge itself runs
	// under the store's lock — two goroutines writing one map is `concurrent
	// map writes`, which is fatal rather than merely wrong.
	if rec.graphBinding != nil {
		rec, err = a.channelSetups.completeGraphHandoff(rec.token, owner, rec.handoffState, derived)
	} else {
		rec, err = a.channelSetups.update(rec.token, owner, func(p *pendingChannelSetup) {
			p.handoffDerived = derived
			if p.handoffAnswers == nil {
				p.handoffAnswers = map[string]string{}
			}
			for k, v := range derived {
				p.handoffAnswers[k] = v
			}
		})
	}
	if err != nil {
		// The exchange happened and its result cannot be stored — an App now
		// exists that nothing here can finish wiring. Loud, and it names the
		// keys so the operator can finish by hand.
		a.cfg.Logger.Info("admind: a channel handoff exchanged but its setup was gone before the result could be stored",
			"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind,
			"derivedKeys", sortedKeys(derived), "err", err.Error())
		writeJSONError(w, http.StatusBadRequest,
			"the browser step completed but this channel setup has expired, so its result could not be kept; "+
				"the external resource it created still exists — see the operator log for what was made")
		return
	}
	a.cfg.Logger.Info("admind: channel handoff exchanged",
		"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind,
		"subject", owner.String(), "derivedKeys", sortedKeys(derived))

	resp := channelHandoffCallbackResponse{
		SetupToken: rec.token,
		Name:       rec.required.Name,
		Namespace:  rec.namespace,
		Kind:       rec.required.Kind,
		Exchanged:  true,
	}
	// Asked of the spec rather than filtered here: which questions an exchange
	// leaves outstanding is a semantic of the contract — a question the
	// handoff answers under ANOTHER name is still answered (github's
	// private-key-path against its private-key) — and a client that decided it
	// for itself would ask a different set than the other one.
	remaining := rec.handoff.Unanswered(derived)
	resp.Questions = unansweredChannelQuestions(remaining, rec.seeded, rec.handoffAnswers)
	if len(resp.Questions) > 0 && rec.handoff.FallbackGuidance != nil {
		// Called ONLY because a question is actually going to be asked: the
		// contract promises that, and a kind may do real work inside it.
		guidance, err := rec.handoff.FallbackGuidance(copyAnswers(rec.handoffAnswers))
		switch {
		case err != nil:
			// Not fatal — the questions are the floor that always works — and
			// not silent either: without the guidance the operator faces bare
			// prompts for values they cannot guess.
			safeErr := rec.redactor.string(err.Error())
			a.cfg.Logger.Info("admind: the guidance for a handoff's remaining questions could not be built",
				"namespace", rec.namespace, "channel", rec.required.Name, "err", safeErr)
			resp.GuidanceError = safeErr
		default:
			if leaked := guidanceLeak(rec.handoff, derived, guidance); leaked != "" {
				a.cfg.Logger.Info("admind: a handoff's guidance carried a value the exchange minted, and was withheld",
					"namespace", rec.namespace, "channel", rec.required.Name, "kind", rec.required.Kind, "answer", leaked)
				resp.GuidanceError = "the instructions for these questions were withheld: the " + rec.required.Kind +
					" kind built them around " + leaked + ", which the browser step minted and which must not reach a browser. " +
					"The values are in the cluster; finish from a terminal with `oap channel create` if you need them"
				break
			}
			resp.Guidance = rec.redactor.string(guidance)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// guidanceLeak names the derived answer whose VALUE appears verbatim in a
// kind's fallback guidance and must not, or "" when the guidance is clean.
//
// WHY THIS EXISTS. Complete runs server-side precisely so an App's private key
// and webhook secret never enter a browser — and then the guidance is built
// from those same answers and put in an HTTP response. That is the identical
// argument this package already makes one file over about
// WizardOutput.Summary, which is logged and not returned "because echoing it
// would make this route's safety a property of every kind's future edits".
// Same reasoning, same obligation, and over HTTP the exposure is new: the CLI
// hands the same guidance to a terminal.
//
// WHAT IT MAY NOT SUBTRACT. A blanket "no derived value may appear" is wrong
// and would break the happy path: github's post-exchange guidance is the
// install URL for the App that was just created, and the App's SLUG is a
// derived answer. So the rule comes from the kind's own declaration — a
// derived answer may appear only if the kind declares a NON-SECRET fallback
// question under that same name, which is the kind saying "this is a value an
// operator types and reads". `slug` and `app-id` are QString questions and are
// publishable; `webhook-secret` is QSecret and is not; the PEM comes back under
// `private-key`, which the kind declares no question for at all (its manual
// route asks for a `private-key-path` instead), so it is not publishable either.
//
// An empty derived value is skipped: it appears in every string.
func guidanceLeak(spec *channelkinds.HandoffSpec, derived map[string]string, guidance string) string {
	publishable := make(map[string]bool, len(spec.FallbackInputs))
	for _, q := range spec.FallbackInputs {
		if q.Type != oap.QSecret {
			publishable[q.Name] = true
		}
	}
	// Sorted, so a guidance carrying two leaked values names the same one every
	// time rather than whichever the map happened to yield first.
	for _, k := range sortedKeys(derived) {
		v := derived[k]
		if v == "" || publishable[k] {
			continue
		}
		if strings.Contains(guidance, v) {
			return k
		}
	}
	return ""
}

// channelHandoffCallbackURL is the address the external service must send the
// operator's browser back to: admind's own callback route, on the origin webd
// publishes, under the prefix adminui proxies it at.
//
// It comes from the PLAN's external base URL rather than from anything the
// request said, because that is the same value the kind's own manifests are
// built against (github puts the webhook URL there) and because a callback a
// caller could name is an open redirect with a credential exchange behind it.
//
// A cluster that has published no external URL cannot do this AT ALL, and the
// refusal is the plan's own sentence: an App created against an empty callback
// is created and then unreachable.
func (a *Admind) channelHandoffCallbackURL(rec *pendingChannelSetup) (string, error) {
	base := strings.TrimSpace(rec.seeded[wizardkeys.KeyExternalBaseURL])
	if base == "" {
		return "", fmt.Errorf(
			"the browser step needs a URL this cluster is reachable at, and none is published: %s",
			channelSetupNoBaseURLReason(rec))
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("the cluster's published external URL %q is not a URL: %w", base, err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + BrowserAPIPathPrefix +
		strings.TrimPrefix(channelHandoffCallbackPath, APIPathPrefix)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// channelSetupNoBaseURLReason is the planner's own words for why no external
// base URL was seeded, or a generic sentence when it recorded none.
func channelSetupNoBaseURLReason(rec *pendingChannelSetup) string {
	if why := rec.notSeeded[wizardkeys.KeyExternalBaseURL]; why != "" {
		return why
	}
	return "webd publishes it once the cluster has an external address"
}

// urlWithHandoffState puts this run's nonce on the address the operator is
// sent to.
//
// On the QUERY STRING, which is where both flows this has to serve round-trip
// it: GitHub's App-manifest endpoint echoes it onto the redirect, and `state`
// is the OAuth authorization endpoint's own spelling of the same thing. The
// kind neither generates nor checks it — the client owns the nonce because the
// client is what verifies the callback.
func urlWithHandoffState(raw, nonce string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("handoff: the kind's address %q is not a URL: %w", raw, err)
	}
	q := u.Query()
	q.Set("state", nonce)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// copyAnswers is a shallow copy, for handing a kind a map it cannot write back
// through.
func copyAnswers(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// replayHandoff is the HandoffDriver for a round trip that ALREADY happened:
// the callback exchanged the code, and the submit request that follows it
// finishes the run.
//
// It exists so the post-callback path goes through the same wizardrun.Finish as
// every other — collision check, handoff, Resolve, Result — rather than
// injecting the exchange's answers from the side. Resolve reads what the
// handoff produced (github's Resolve turns a key PATH into the key), so an
// order that skipped the handoff position would be a different run.
func replayHandoff(derived map[string]string) wizardrun.HandoffDriver {
	return func(_ context.Context, _ *channelkinds.HandoffSpec, _ map[string]string) (map[string]string, error) {
		return derived, nil
	}
}

// sortedKeys names what an exchange produced, for a log line, WITHOUT the
// values: github's are the App's private key and webhook secret.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
