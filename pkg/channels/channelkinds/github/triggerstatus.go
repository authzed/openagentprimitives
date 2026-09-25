package github

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/checkruns"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
)

// Compile-time interface check.
var _ channelkinds.TriggerStatusReporter = Kind{}

// TriggerSurfaceKind names, generically, what this kind reports on. Pure — no
// Channel, no credentials, no I/O — because its caller is tool assembly, which
// decides whether to offer the trigger-status tools from the binding's kind
// name alone and must not pay a provider round trip to write a description.
func (Kind) TriggerSurfaceKind() string { return "a pull request's GitHub check run" }

// TriggerSurface implements channelkinds.TriggerStatusReporter: it returns a
// handle on the check run for the pull request this session is bound to.
//
// Everything it needs is already on the Channel and the binding — the App slug
// names the check run, and the binding key names the repository and the pull
// request, both written by this same kind's Translate when the webhook arrived.
// Nothing is taken from a caller, which is the point: an agent driving this
// surface supplies a verdict and nothing else.
//
// No I/O here, per the seam's contract. Credentials are extracted and validated
// (so a Secret missing a key is refused at assembly, where an operator sees it,
// rather than at the end of a review), but nothing is minted and nothing is
// called until the returned surface's own methods run.
func (Kind) TriggerSurface(
	ch *spiceboxv1alpha1.Channel,
	b *spiceboxv1alpha1.ChannelBinding,
	secrets channelkinds.WebhookSecrets,
	opts channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	if ch == nil || b == nil {
		return nil, fmt.Errorf("github: trigger status needs both a Channel and a binding")
	}
	// spec.github can be nil on a malformed Channel (ValidateSpec rejects that
	// shape). Refused rather than reported as "no trigger here": a github
	// session always has a pull request behind it, so silence would leave the
	// agent with no way to answer and nobody told why.
	if ch.Spec.GitHub == nil {
		return nil, fmt.Errorf("github: Channel %s/%s has no spec.github, so its check run cannot be named",
			ch.Namespace, ch.Name)
	}
	owner, repo, number, err := parseChannelKey(b.Key)
	if err != nil {
		return nil, fmt.Errorf("github: Channel %s/%s: %w", ch.Namespace, ch.Name, err)
	}

	appID := string(secrets.Data["app-id"])
	privateKey := secrets.Data["private-key"]
	installationID := string(secrets.Data["installation-id"])
	var missing []string
	if appID == "" {
		missing = append(missing, "app-id")
	}
	if len(privateKey) == 0 {
		missing = append(missing, "private-key")
	}
	if installationID == "" {
		missing = append(missing, "installation-id")
	}
	if len(missing) > 0 {
		// Names the keys and nothing else. The private key in particular must
		// never reach an error string: these travel to logs and to tool
		// results.
		return nil, fmt.Errorf("github: Channel %s/%s: credentials Secret has no %s, so no installation token can be minted for its check run",
			ch.Namespace, ch.Name, strings.Join(missing, ", "))
	}

	return &checkRunSurface{
		repo:           checkruns.Repo{Owner: owner, Name: repo},
		number:         number,
		name:           checkRunName(ch),
		appID:          appID,
		privateKey:     privateKey,
		installationID: installationID,
		minter:         githubapp.NewHTTPMinter(githubapp.WithBaseURL(providerAPIBaseURL(opts))),
		api:            checkruns.New(checkruns.WithBaseURL(opts.ProviderAPIBaseURL)),
	}, nil
}

// providerAPIBaseURL resolves the minter's host. The minter's own default is
// the real API, and its WithBaseURL takes the value verbatim, so an empty
// override has to be turned back into "leave the default alone" here.
func providerAPIBaseURL(opts channelkinds.TriggerStatusOptions) string {
	if opts.ProviderAPIBaseURL == "" {
		return "https://api.github.com"
	}
	return opts.ProviderAPIBaseURL
}

// checkRunName is the name the check run carries on the pull request.
//
// The App's slug, so the check reads as coming from the app a maintainer
// installed, and so this kind can find its OWN run again on a redelivery
// without matching on anything a person might have typed. Falls back to the
// Channel name only for a spec that somehow carries no slug, which keeps the
// find-or-create key non-empty rather than matching every unnamed run.
func checkRunName(ch *spiceboxv1alpha1.Channel) string {
	if slug := ch.Spec.GitHub.AppSlug; slug != "" {
		return slug
	}
	return ch.Name
}

// checkRunSurface is one pull request's check run, as this kind reports on it.
type checkRunSurface struct {
	repo   checkruns.Repo
	number int
	name   string

	appID          string
	privateKey     []byte
	installationID string

	minter githubapp.Minter
	api    *checkruns.Client
}

var _ channelkinds.TriggerSurface = (*checkRunSurface)(nil)

// Surface names this pull request's check run for a reader. Pure and free —
// it is called to build a tool result and a notice, and either would be a poor
// place for a provider round trip.
func (s *checkRunSurface) Surface() string {
	return fmt.Sprintf("the GitHub check run on %s#%d", s.repo, s.number)
}

// Claim opens the check run for this pull request's current head, or reports
// the answer already on it.
//
// The ordering matters and is not cosmetic. It reads first: a completed run for
// this commit means the pull request was already reviewed at this SHA and the
// delivery that woke this session is a repeat, so claiming again would replace
// a finished answer with "in progress". An in-progress run means a claim is
// already open — a concurrent delivery, or this session restarting — and a
// second one would leave two runs on the commit with no way for the next
// delivery to tell which is ours.
func (s *checkRunSurface) Claim(ctx context.Context) (channelkinds.TriggerClaim, error) {
	token, sha, existing, err := s.locate(ctx)
	if err != nil {
		return channelkinds.TriggerClaim{}, err
	}
	if existing != nil {
		if existing.Completed() {
			return channelkinds.TriggerClaim{
				Ref:       s.ref(existing.ID),
				Concluded: true,
				Outcome:   outcomeFromConclusion(existing.Conclusion),
			}, nil
		}
		return channelkinds.TriggerClaim{Ref: s.ref(existing.ID)}, nil
	}

	run, err := s.api.Create(ctx, token, s.repo, checkruns.Write{
		Name:      s.name,
		HeadSHA:   sha,
		Status:    "in_progress",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return channelkinds.TriggerClaim{}, fmt.Errorf("github: open check run on %s#%d: %w", s.repo, s.number, err)
	}
	return channelkinds.TriggerClaim{Ref: s.ref(run.ID)}, nil
}

// Conclude writes the final answer onto the check run.
//
// Find-or-create, so it does not depend on Claim having run. That is what makes
// a missed claim harmless (the verdict still lands) and what lets a caller with
// no agent at all — an operator closing out a session that died — write an
// answer for a pull request nothing ever claimed.
func (s *checkRunSurface) Conclude(ctx context.Context, c channelkinds.TriggerConclusion) error {
	conclusion, err := conclusionFor(c.Outcome)
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.Summary) == "" {
		return fmt.Errorf("github: a check run conclusion needs a summary; an empty one tells a reader of %s#%d nothing", s.repo, s.number)
	}
	if err := validDetailsURL(c.DetailsURL); err != nil {
		return err
	}

	token, sha, existing, err := s.locate(ctx)
	if err != nil {
		return err
	}
	write := checkruns.Write{
		Status:      "completed",
		Conclusion:  conclusion,
		DetailsURL:  c.DetailsURL,
		CompletedAt: time.Now().UTC().Format(time.RFC3339),
		// The commit this answer is about, recorded by the kind. A later round
		// reads it back to diff against what was already reviewed — a fact the
		// agent used to have to carry and restate.
		ExternalID: sha,
		Output: &checkruns.Output{
			Title:   titleFor(c.Outcome),
			Summary: boundSummary(c.Summary),
		},
	}
	if existing != nil {
		if err := s.api.Update(ctx, token, s.repo, existing.ID, write); err != nil {
			return fmt.Errorf("github: conclude check run on %s#%d: %w", s.repo, s.number, err)
		}
		return nil
	}
	write.Name = s.name
	write.HeadSHA = sha
	if _, err := s.api.Create(ctx, token, s.repo, write); err != nil {
		return fmt.Errorf("github: record check run on %s#%d: %w", s.repo, s.number, err)
	}
	return nil
}

// locate mints a token, resolves the pull request's current head, and finds
// this kind's own check run on it. Shared by Claim and Conclude so both address
// the SAME commit by the same route — a conclusion landing on a different SHA
// than the claim would silently answer for a commit nobody reviewed.
func (s *checkRunSurface) locate(ctx context.Context) (token, sha string, existing *checkruns.Run, err error) {
	minted, err := s.minter.Mint(ctx, githubapp.MintRequest{
		AppID:          s.appID,
		PrivateKeyPEM:  s.privateKey,
		InstallationID: s.installationID,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("github: mint installation token for %s: %w", s.repo, err)
	}
	// UnderlyingValue as late as possible, and into a local that never leaves
	// this call: SensitiveValue exists so a stray %v or a marshalled struct
	// redacts, and unwrapping it early would give that up for the whole surface.
	token = string(minted.AccessToken.UnderlyingValue())

	sha, err = s.api.PullRequestHeadSHA(ctx, token, s.repo, s.number)
	if err != nil {
		return "", "", nil, fmt.Errorf("github: read head commit of %s#%d: %w", s.repo, s.number, err)
	}
	existing, err = s.api.FindByName(ctx, token, s.repo, sha, s.name)
	if err != nil {
		return "", "", nil, fmt.Errorf("github: read check runs on %s#%d: %w", s.repo, s.number, err)
	}
	return token, sha, existing, nil
}

// ref renders a check run id for a human reading a tool result or a log.
//
// checkRunIDPattern below is its INVERSE and the two are pinned together by a
// round-trip test; a reword here that the pattern cannot read back is a
// silent loss of the one provider-minted value a replay has to be told.
func (s *checkRunSurface) ref(id int64) string {
	return fmt.Sprintf("%s%s%s%s%s#%d", s.name, checkRunRefInfix, strconv.FormatInt(id, 10), checkRunRefOn, s.repo, s.number)
}

// The two fixed spans of ref's rendering, named so the pattern that reads them
// back is built from the same strings rather than from a second spelling.
const (
	checkRunRefInfix = " check run "
	checkRunRefOn    = " on "
)

// headCommitLabel prefixes the head commit in the prompt renderPrompt composes.
// Read back by TriggerProviderStateIn; see ref's note on why the pair is one
// fact.
const headCommitLabel = "Head commit: "

// checkRunIDPattern reads back the id out of what ref wrote.
//
// Anchored on BOTH fixed spans rather than on the digits alone, because a
// transcript is full of numbers and a bare `\d+` would file a pull request
// number, a line count or a byte total as a mint — and a mint recorded that
// never happened shifts every later id in the sequence by a position, which is
// strictly worse than missing one.
var checkRunIDPattern = regexp.MustCompile(
	regexp.QuoteMeta(checkRunRefInfix) + `(\d+)` + regexp.QuoteMeta(checkRunRefOn))

// headCommitPattern reads back the head commit out of what renderPrompt wrote.
//
// The hex body is bounded to git's own object-id shape (40 for SHA-1, 64 for
// SHA-256) so the label alone cannot capture a sentence: a prompt that grew a
// line reading "Head commit: unknown" yields nothing here rather than seeding a
// stand-in with a word.
var headCommitPattern = regexp.MustCompile(
	regexp.QuoteMeta(headCommitLabel) + `([0-9a-f]{40}|[0-9a-f]{64})\b`)

// TriggerProviderStateIn implements channelkinds.TriggerStatusReporter.
//
// It reads text THIS KIND wrote — the prompt its receiver rendered for the
// delivery, and the ref its own surface reports — and returns the two values in
// it that came from GitHub rather than from us: the head commit the surface
// resolved off the pull request, and the ids GitHub minted for the check runs
// it opened.
//
// Deliberately NOT a JSON decode of any tool's result. The values are embedded
// in prose the kind composed, and the tool that carries them today is not the
// only place they can land — a notice, a log line and a summary render the same
// ref. Matching the kind's own rendering finds them wherever it appears, and
// keeps this from becoming a rule about one tool's result schema.
func (Kind) TriggerProviderStateIn(text string) channelkinds.TriggerProviderState {
	var out channelkinds.TriggerProviderState
	if m := headCommitPattern.FindStringSubmatch(text); m != nil {
		out.SurfaceRevision = m[1]
	}
	for _, m := range checkRunIDPattern.FindAllStringSubmatch(text, -1) {
		out.MintedIDs = append(out.MintedIDs, m[1])
	}
	return out
}

// conclusionFor maps the framework's outcome vocabulary onto github's.
//
// This mapping is the whole reason the framework has its own words. `neutral`
// for could_not_finish rather than `failure` is deliberate: a review that could
// not run is not a statement that the change is bad, and a red X would be read
// as one by every maintainer who saw it.
func conclusionFor(o channelkinds.TriggerOutcome) (string, error) {
	switch o {
	case channelkinds.TriggerOutcomeClean:
		return "success", nil
	case channelkinds.TriggerOutcomeProblemsFound:
		return "action_required", nil
	case channelkinds.TriggerOutcomeCouldNotFinish:
		return "neutral", nil
	default:
		// Re-derives the refusal from the shared parser so this kind cannot
		// disagree with the seam about what the legal outcomes are.
		if _, err := channelkinds.ParseTriggerOutcome(string(o)); err != nil {
			return "", fmt.Errorf("github: cannot conclude a check run: %w", err)
		}
		return "", fmt.Errorf("github: cannot conclude a check run: outcome %q has no github conclusion", o)
	}
}

// outcomeFromConclusion reads a github conclusion back into the framework's
// vocabulary, for reporting an answer this kind (or a previous version of it)
// already wrote.
//
// Anything outside the three this kind writes — a run a maintainer cancelled,
// a `timed_out`, a `failure` from an older build — reads as could_not_finish.
// That is the honest reading: the commit carries an answer, and the answer is
// not "reviewed clean".
func outcomeFromConclusion(conclusion string) channelkinds.TriggerOutcome {
	switch conclusion {
	case "success":
		return channelkinds.TriggerOutcomeClean
	case "action_required":
		return channelkinds.TriggerOutcomeProblemsFound
	default:
		return channelkinds.TriggerOutcomeCouldNotFinish
	}
}

// titleFor is the one-line heading github shows above the summary. Derived from
// the outcome rather than taken from the caller: the heading is the part a
// maintainer reads at a glance in a list of checks, and it must say what the
// check concluded, not what the agent chose to call it.
func titleFor(o channelkinds.TriggerOutcome) string {
	switch o {
	case channelkinds.TriggerOutcomeClean:
		return "No blocking findings"
	case channelkinds.TriggerOutcomeProblemsFound:
		return "Findings need attention"
	default:
		return "Could not complete"
	}
}

// validDetailsURL refuses a link this kind will not publish.
//
// The details URL is rendered as a clickable link under the operator's own App,
// to maintainers who chose to trust the App and not the agent. Only http(s)
// gets through, and only an absolute URL with a host — a scheme-relative or
// relative value would resolve against github.com and point a reader somewhere
// they would reasonably read as github's own.
func validDetailsURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("github: details URL is not a URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("github: details URL must be an absolute http(s) URL; %q is not", raw)
	}
	return nil
}

// maxSummaryRunes bounds the agent-authored summary published on the pull
// request.
//
// GitHub's own ceiling for a check run's output.summary is 65535 characters and
// it REJECTS an over-long body rather than trimming it — so an unbounded
// summary does not merely flood a pull request, it turns a finished review into
// a check run that never concludes, which is the exact failure this seam exists
// to end. Measured in runes so a cut cannot land mid-rune.
const maxSummaryRunes = 60000

// summaryTruncationMarker replaces what was cut. Visible on purpose: a reader
// puzzling over a summary that stops mid-sentence should be told that the
// prompt, not the review, is where it ended.
const summaryTruncationMarker = "\n\n_[…truncated to %d of %d characters]_"

// boundSummary caps the published summary, marking the cut.
func boundSummary(s string) string {
	n := utf8.RuneCountInString(s)
	if n <= maxSummaryRunes {
		return s
	}
	return string([]rune(s)[:maxSummaryRunes]) + fmt.Sprintf(summaryTruncationMarker, maxSummaryRunes, n)
}
