package channelkinds

import (
	"context"
	"fmt"
	"slices"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TriggerStatusReporter is WebhookReceiver's REPORTING counterpart.
//
// WebhookReceiver is "this kind receives inbound over HTTP". This is "the
// event that received it has a status surface on the provider, and this kind
// owns it": a GitHub pull request's check run today. A kind implements it when
// its trigger carries a place the provider expects an answer to be written
// back to; kinds whose trigger is just a message do not implement it and are
// unaffected.
//
// Capability is declared by implementing, never by a list a consumer sweeps —
// the same shape as WebhookReceiver and WebAuthenticator. The one lookup is
// registry.TriggerStatusReporterFor, which every consumer resolves through.
//
// # Why this is a kind capability and not prompt text
//
// Everything the surface needs to be addressed — repository, pull request,
// head commit — arrived on the webhook this same kind translated. An agent
// told to drive that surface with raw API calls has to carry those facts in
// prose and re-assemble them, and every step it can assemble it can also skip:
// a session that ended one step early once left a pull request showing a
// review permanently "in progress" that nobody was performing. The kind
// already holds the identifiers, so the identifiers are not the agent's to
// supply — only the judgement is.
//
// # Reachable without an agent, on purpose
//
// A TriggerSurface is built from a Channel, a binding and that Channel's
// Secret, and from nothing session-runtime. That is what lets a caller with no
// running agent — an operator reconciling a session that failed before its
// runner ever started, or one closing a claim a finished session left open —
// resolve the same surface and conclude it. An implementation that needed
// state only the runner holds would foreclose both.
type TriggerStatusReporter interface {
	// TriggerSurfaceKind names, generically, what this kind reports on — "a
	// pull request's GitHub check run". It is PROMPT COPY: it lands in the
	// description of the tools offered for this kind, which is why it belongs
	// to the kind and not to the tool.
	//
	// PURE. No Channel, no credentials, no I/O: its caller is tool assembly,
	// which resolves this from a binding's kind name alone and must not pay a
	// provider round trip to write a sentence.
	TriggerSurfaceKind() string

	// TriggerSurface returns a handle on the status surface of the trigger
	// that started the session bound by b, for the Channel that received it.
	//
	// (nil, nil) means this binding carries no trigger this kind reports on —
	// a normal answer, not a failure, and callers stay silent on it. A non-nil
	// error means the surface exists but could not be addressed (a Secret
	// missing the keys the provider API needs, a binding key this kind cannot
	// parse); callers report those rather than treating them as absence.
	//
	// Implementations MUST NOT perform I/O here. Resolving credentials and
	// talking to the provider belong in the returned surface's own calls, so a
	// caller deciding only WHETHER a surface exists pays nothing.
	TriggerSurface(ch *spiceboxv1alpha1.Channel, b *spiceboxv1alpha1.ChannelBinding, secrets WebhookSecrets, opts TriggerStatusOptions) (TriggerSurface, error)

	// TriggerProviderStateIn reads back, out of TEXT THIS KIND ITSELF WROTE
	// during a run, the values that run got from the PROVIDER rather than from
	// us — the revision its surface resolved off the triggering resource, and
	// the identifiers the provider minted for it.
	//
	// # Why the kind answers this, and not its reader
	//
	// Every one of those values reaches a durable record only inside a string
	// this kind composed: the head commit inside the prompt its receiver
	// rendered, the minted id inside the ref its surface reports. A reader that
	// recognized them would be spelling this kind's own formats somewhere else,
	// and the two spellings would drift the first time either was reworded. So
	// the formatter and its inverse live together in the kind's own package,
	// where a round-trip test can pin the pair.
	//
	// # What it is FOR
	//
	// A replay of a recorded run drives this kind against a STAND-IN provider,
	// which holds none of the state the real one held and mints identifiers of
	// its own. Seeding the stand-in with these values is what lets this kind's
	// real code — the claim, the create, the patch-by-id, the whole round trip
	// — run and compose the same answer it composed the first time. Only the
	// values the provider itself chose are pinned; nothing our code does is
	// stood in for.
	//
	// PURE, and total. Text carrying none of this yields the zero value, which
	// is the ordinary answer for the great majority of any transcript, and is
	// never an error: this reads prose, and prose that says nothing about a
	// trigger is not a failure.
	TriggerProviderStateIn(text string) TriggerProviderState
}

// TriggerProviderState is what a run OBSERVED from a provider — the values that
// crossed in from outside our code and that a stand-in must therefore be told,
// because it cannot derive them.
//
// The distinction it draws is the whole point. A meta tool's reply mixes values
// our code composed with values a provider minted or held. Our half is
// reproduced by RUNNING our code at replay; only this half has to be carried.
type TriggerProviderState struct {
	// SurfaceRevision is the revision of the triggering resource the status
	// surface answers for — for GitHub, the pull request's head commit, which
	// the surface resolves by READING the pull request rather than from
	// anything the agent carried.
	//
	// Empty when the text names none, which is the ordinary case.
	SurfaceRevision string

	// MintedIDs are identifiers the PROVIDER minted, in the order the text
	// presents them — for GitHub, the check run ids it assigned.
	//
	// Order is the contract, not a convenience: a stand-in hands them back in
	// this order as its own mints, so a run that mints them in a different
	// order addresses a different object than the recorded one did.
	MintedIDs []string
}

// Merge folds other into s, keeping the FIRST revision seen and appending
// ids not already present.
//
// First-wins on the revision because a transcript is read in order and the
// earliest statement of it is the one the surface resolved before any work
// happened; dedup on the ids because a later text echoing an id back — a
// summary, a log line — is not a second mint, and counting it as one would
// shift every id after it by a position.
func (s TriggerProviderState) Merge(other TriggerProviderState) TriggerProviderState {
	if s.SurfaceRevision == "" {
		s.SurfaceRevision = other.SurfaceRevision
	}
	for _, id := range other.MintedIDs {
		if !slices.Contains(s.MintedIDs, id) {
			s.MintedIDs = append(s.MintedIDs, id)
		}
	}
	return s
}

// TriggerStatusOptions carries the caller-side overrides a TriggerSurface
// needs. Mirrors the providerAPIBaseURL parameter WebhookURLDriftChecker
// already takes, for the same reason: without a seam pointing the outbound
// call somewhere else, nothing below this line is testable against anything
// but the real provider.
type TriggerStatusOptions struct {
	// ProviderAPIBaseURL overrides the provider's API host (a test's
	// stand-in server, or a self-hosted instance). Empty means the
	// implementation's real default. Production leaves it empty.
	ProviderAPIBaseURL string
}

// TriggerSurface is one session's handle on its trigger's status.
//
// Both calls are IDEMPOTENT and self-locating: neither takes an identifier,
// because an implementation derives every identifier from the binding it was
// built with. Conclude in particular must not depend on Claim having run — a
// session that never claimed, or a caller concluding on behalf of one that
// died, has to be able to write the answer anyway.
type TriggerSurface interface {
	// Surface names what is being reported on, in the reader's terms ("this
	// pull request's GitHub check run"). It is MODEL- and HUMAN-facing copy:
	// it lands in a tool description and in the text a person reads, so it
	// names the thing the reader can go look at and never an internal object.
	Surface() string

	// Claim marks the trigger as being worked on now, and reports whether it
	// already carries an answer.
	//
	// Idempotent: claiming an already-claimed trigger re-reports the existing
	// claim rather than opening a second one, so a redelivery cannot fan the
	// surface out.
	Claim(ctx context.Context) (TriggerClaim, error)

	// Conclude writes the final answer. Find-or-create: it targets an existing
	// claim when there is one and opens an already-concluded status when there
	// is not, so forgetting to Claim cannot strand anything and cannot lose the
	// conclusion either.
	Conclude(ctx context.Context, c TriggerConclusion) error
}

// TriggerClaim is what Claim observed and did.
type TriggerClaim struct {
	// Ref is the provider-side handle of the claimed status, for a human
	// reading a log or a tool result. Opaque; nothing routes on it.
	Ref string

	// Concluded reports that this trigger ALREADY carries a conclusion — the
	// same commit was answered before, so this delivery is a repeat. Claim
	// writes nothing in that case.
	Concluded bool

	// Outcome is the conclusion already on the surface. Meaningful only when
	// Concluded.
	Outcome TriggerOutcome
}

// TriggerOutcome is the FRAMEWORK's vocabulary for how a round of work turned
// out. Deliberately not any provider's: each kind maps these onto its own
// (github's check-run conclusions are success / action_required / neutral),
// so a second kind can join without the agent-facing words changing and
// without a consumer learning a provider's enum.
type TriggerOutcome string

const (
	// TriggerOutcomeClean is "I did the work and found nothing that should
	// block".
	TriggerOutcomeClean TriggerOutcome = "clean"

	// TriggerOutcomeProblemsFound is "I did the work and it should not proceed
	// as-is". It is a verdict on the work, not a failure of the agent.
	TriggerOutcomeProblemsFound TriggerOutcome = "problems_found"

	// TriggerOutcomeCouldNotFinish is "I could not do the work" — a refusal, a
	// failed checkout, an exhausted budget, or a session that died before it
	// ever ran. It is the only outcome a caller with no agent can honestly
	// write, which is why it exists separately from problems_found.
	TriggerOutcomeCouldNotFinish TriggerOutcome = "could_not_finish"
)

// TriggerOutcomes returns the whole closed set, in the order a reader should
// meet them. It is what a tool's JSON schema enumerates and what a refusal
// names, so both are derived from one list rather than retyped.
func TriggerOutcomes() []TriggerOutcome {
	return []TriggerOutcome{
		TriggerOutcomeClean,
		TriggerOutcomeProblemsFound,
		TriggerOutcomeCouldNotFinish,
	}
}

// ParseTriggerOutcome validates a caller-supplied outcome.
//
// Fail-closed on anything unrecognized, INCLUDING empty: an outcome is the one
// judgement the framework cannot make on the agent's behalf, and defaulting it
// would silently publish a verdict nobody reached. The error names the legal
// values because its most common reader is a model correcting its own call.
func ParseTriggerOutcome(s string) (TriggerOutcome, error) {
	for _, o := range TriggerOutcomes() {
		if string(o) == s {
			return o, nil
		}
	}
	legal := make([]string, 0, len(TriggerOutcomes()))
	for _, o := range TriggerOutcomes() {
		legal = append(legal, string(o))
	}
	return "", fmt.Errorf("%q is not a valid outcome; use one of: %s", s, strings.Join(legal, ", "))
}

// TriggerConclusion is the answer written back to the trigger's surface.
type TriggerConclusion struct {
	// Outcome is the verdict. Required; a zero value is a caller bug and an
	// implementation must refuse it rather than pick one.
	Outcome TriggerOutcome

	// Summary is the prose a person reads on the provider's surface.
	//
	// UNTRUSTED when an agent wrote it: it is agent output posted outward,
	// often to a public repository. An implementation is responsible for
	// bounding it and for placing it where the provider treats it as content,
	// never as configuration.
	Summary string

	// DetailsURL optionally points at where the full result lives (the thread
	// the review was delivered into). Implementations MUST refuse a scheme
	// other than http/https: this is a link published under the operator's own
	// app, to readers who did not choose to trust the agent.
	DetailsURL string
}
