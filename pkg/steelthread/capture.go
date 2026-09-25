package steelthread

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The on-disk names WriteResult emits. bundleFileName in particular is what the
// bronzethread suite DISCOVERS a scenario by, so a capture that wrote its
// bundle under any other name produces a directory the suite silently skips —
// which reads as a passing suite.
const (
	bundleFileName = "bundle.json"
	goldenFileName = "trace.golden"
	payloadsDir    = "payloads"

	// triggerPayloadFileName is the one file whose path the bundle itself
	// states (bt.Trigger.Payload). A fixed name rather than a derived one: the
	// delivery that opened a session is singular, and a name derived from the
	// event would make two captures of the same agent differ for no reason.
	triggerPayloadFileName = "delivery.json"

	// bootstrapFileName carries the derived SpiceDB seed. Numbered after
	// 03-agent.yaml because the harness applies a directory's *.yaml in sorted
	// order and the relationships describe objects the class refers to.
	bootstrapFileName = "04-bootstrap.yaml"

	// bootstrapName is the SpiceDBBootstrap CR's metadata.name. Fixed, like
	// every other rewrite value, so a re-capture is byte-identical.
	bootstrapName = "captured-seed"

	// wildcardSubjectID is SpiceDB's match-any subject id. The CRD spells it as
	// a boolean instead, so a recorded "<type>:*" has to be translated.
	wildcardSubjectID = "*"
)

// CaptureInput is everything a capture needs that its records cannot say.
//
// Now and OapVersion are carried rather than read from the clock and the build
// stamp inside Capture, and that is the whole reason a determinism test is
// possible: two captures of one session must produce byte-identical output, or
// the re-capture diff is noise and nobody reads it. It is the same reasoning
// that keeps a volatile value out of an SSA-applied field.
type CaptureInput struct {
	// Name is the bundle's name, and also the directory the emitted AgentDir
	// points at. Required.
	Name string
	// Description is the prose a human writes about what the scenario proves.
	// Empty is normal: a capture has no opinion about why the session mattered.
	Description string
	// Session is the namespace/name the transcript came from, and Cluster the
	// kubecontext it came from — provenance for a reader trying to find it
	// again.
	Session string
	Cluster string
	// OapVersion is the oap build that produced the bundle.
	OapVersion string
	// Now is when the capture ran, NOT when the session ran.
	Now time.Time

	// Fixture is the live agent's manifests. Class is required: it names the
	// bundle's AgentClass, declares the MCP prefixes the fold reads, and
	// carries the tool-guard policy GuardConfigured is derived from.
	Fixture FixtureInput

	// Expand resolves an allowed permission into the relationship tree SpiceDB
	// used to answer it. Required — see Capture on why a nil one is an error
	// rather than a seed of zero tuples.
	Expand Expander

	// SessionGrants is the AgentSessionGrants CR the session's own AgentClass
	// owns — the class's declaration of which (resourceType, permission) pairs
	// are grant PAIRS and which are SLOTS. Read by DeriveSeed for the slots
	// half; see collectedSlotGrant for what it decides.
	//
	// Deliberately NOT part of Fixture. Everything in FixtureInput is a
	// manifest RewriteFixture emits into the bundle directory, and this one
	// must never be emitted: the replayed AgentClass reconciler writes its own
	// from the class spec, so a captured copy would be a second, frozen source
	// of truth for the same declaration.
	//
	// Nil is legitimate — an unreconciled or pre-CR class — and is the STRICT
	// answer, not a permissive one. See declaredSlots.
	SessionGrants *spiceboxv1alpha1.AgentSessionGrants

	// MetaTools are the LLM-facing names that RUN FOR REAL at replay, gathered
	// from the same capability assembly the runner drives. See
	// SelfCheckInput.MetaTools: an empty list is fail-closed, and every non-MCP
	// call then reads as a sandbox call.
	MetaTools []string

	// FixtureTools are the meta tools the REWRITTEN fixture will offer — the
	// same assembly as MetaTools, run against what ReplayChannel returns rather
	// than against the live cluster. See SelfCheckInput.FixtureTools: this is
	// the half that can see a tool the rewrite destroyed, and leaving it empty
	// raises CodeFixtureToolsNotComputed rather than passing.
	FixtureTools []FixtureTool

	// TriggerProvider is the input Channel kind's own trigger-status reporter,
	// resolved through the channel-kind registry, or nil when that kind reports
	// no trigger status.
	//
	// THE only thing that can read a provider's own values back out of the
	// transcript, because the values live inside text that kind composed. See
	// providerstate.go: this package supplies which text to read and never how
	// to read it.
	//
	// Nil is a legitimate answer and not a gap — most sessions have no trigger
	// at all. What is NOT legitimate is a session whose recorded run called a
	// provider-surface tool with no reporter supplied, and
	// CodeProviderStateNotComputed catches exactly that.
	TriggerProvider channelkinds.TriggerStatusReporter

	// MentionStandIn is the user directory the emitted fixture's fake kind will
	// be seeded with, or nil when the rewrite took no directory away.
	//
	// Supplied by the caller rather than derived here for the reason FixtureTools
	// is: only the caller runs the capability assembly twice and can see what the
	// seed put back. Recorded on the bundle so the replay seeds precisely what
	// the prediction was made against — a driver seeding something else would
	// offer a tool set the capture never predicted.
	MentionStandIn *bt.MentionStandIn

	// FakeDeliverySurfaces turns the fake kind's artifact-delivery capabilities
	// on for the emitted bundle. Supplied by the caller for the reason
	// MentionStandIn is: it is a comparison between the LIVE bound kind and the
	// one the rewrite emits, and only the caller holds both.
	//
	// The third thing the rewrite to kind=fake takes away, after a provider
	// surface and a directory, and the one with a bundle field already waiting
	// for it. Without it a run that delivered an artifact replays into
	// respond_to_user refusing `attached` — our own code, correctly, about a
	// capability the recorded channel really had.
	FakeDeliverySurfaces bool

	// LiveSecrets are the credential values read while gathering the manifests,
	// carried for the sole purpose of proving their absence from the emitted
	// files. See SelfCheckInput.LiveSecrets: leaving it empty does not pass the
	// scan, it raises CodeSecretCheckSkipped.
	LiveSecrets []LiveSecret

	// Redact are the explicit replacements to apply to the emitted bytes before
	// anything is scanned or written. Optional; see Redaction for why they are
	// human-supplied rather than guessed, and Capture for why they run FIRST.
	Redact []Redaction

	// ElideSkills are the operator-declared skill-source substitutions to apply
	// to the FIXTURE, before anything is rewritten or folded.
	//
	// Not a Redaction, and deliberately a separate mechanism: this one is
	// structural (it moves a canonical name's authority and re-derives every
	// object name and owner-ref that referred to it) and it REFUSES rather than
	// eliding a skill the recorded agent could see. See SkillElision.
	ElideSkills []SkillElision
}

// EmittedFile is one file WriteResult writes, carrying the FINAL bytes exactly
// as they land on disk.
//
// The unit the secret scans run over, and it has to be the FINAL bytes rather
// than the objects behind them: a value that only appears after marshalling —
// inside a JSON-escaped tool result, in a field no rewrite rule mentions —
// exists in no object the checks could inspect, and a scan of the in-memory
// structures would look straight past it.
type EmittedFile struct {
	// Name is the path relative to the bundle directory, slash-separated:
	// "bundle.json", "03-agent.yaml", "payloads/delivery.json".
	Name string
	// Bytes are what gets written, byte for byte.
	Bytes []byte
}

// Result is one capture's output, ready to be written.
type Result struct {
	// Bundle is the scenario itself.
	Bundle bt.Bundle
	// Fixture is the manifest set the bundle's AgentDir holds, POST-redaction —
	// the same bytes Emitted carries for those files.
	Fixture []FixtureFile
	// Golden is the frozen authorization trace, post-redaction.
	Golden []byte
	// TriggerPayload is the delivery body a triggered bundle re-signs,
	// post-redaction. Nil for a session a person typed into.
	//
	// Redacting it is safe: the driver signs the payload it reads off disk with
	// the fixture Channel's own Secret at replay time, so the signature is
	// computed over whatever bytes are there. A capture that redacted the body
	// but shipped the ORIGINAL signature is the failure this avoids by not
	// shipping a signature at all.
	TriggerPayload []byte

	// Emitted is every file WriteResult writes, in write order, and it is the
	// ONLY thing WriteResult writes.
	//
	// One list rather than four separate surfaces, because the scans and the
	// writer must not be able to disagree about what lands in the repo. They
	// used to: the leak scan looked at the fixture manifests alone, so
	// bundle.json — which is where a credential a TOOL returned would be — and
	// the trigger payload went out unscanned, and the capture reported clean.
	// Scanning this list is the same act as writing it.
	Emitted []EmittedFile

	// redactionCounts is per-rule hit totals across every surface, parallel to
	// the rules that produced them. Unexported: it is emit's own bookkeeping
	// between its two passes, and what a reader is owed lands in the bundle as
	// bt.Capture.Redactions.
	redactionCounts []int
}

// Capture assembles one session's records into a replayable bundle.
//
// It runs every stage — fold, fixture rewrite, seed derivation, assertions,
// golden trace, self-check — and returns the findings WITHOUT deciding what to
// do about them. The command decides: that keeps the exit-code policy in one
// place and keeps this function testable against synthetic records with no
// cluster, no LLM, and no process exit.
//
// A nil Expand is an error rather than an expansion of nothing. Treating it as
// "this permission grants no tuples" would emit a fixture that denies where the
// run allowed, and that divergence surfaces minutes later inside a replay,
// pointing at the code under test instead of at the capture that mis-seeded it.
func Capture(recs Records, in CaptureInput) (Result, []Finding, error) {
	if in.Name == "" {
		return Result{}, nil, fmt.Errorf("steelthread: Capture: CaptureInput.Name is required")
	}
	if in.Fixture.Class == nil {
		return Result{}, nil, fmt.Errorf("steelthread: Capture: CaptureInput.Fixture.Class is required")
	}
	if in.Expand == nil {
		return Result{}, nil, fmt.Errorf("steelthread: Capture: CaptureInput.Expand is required; " +
			"a capture with no expander would seed nothing and the replay would deny where the run allowed")
	}

	// Before any work, because a malformed or length-changing rule is an
	// operator's typo and the cheapest place to report it is the first thing
	// that happens. Resolving here also means a caller that built Redactions in
	// code rather than through ParseRedaction — every test in this package —
	// gets the same generated stand-ins and the same length refusal the CLI
	// does, instead of a second, weaker contract nobody maintains. Idempotent,
	// so a caller that already resolved loses nothing by passing the result in.
	resolvedRedactions, err := ResolveRedactions(in.Redact)
	if err != nil {
		return Result{}, nil, err
	}
	in.Redact = resolvedRedactions

	// FIRST, and before anything reads in.Fixture. Every later stage derives
	// from these manifests — the fold's tool prefixes, the fixture rewrite, the
	// class's skill refs the self-check compares against what was emitted — so
	// a substitution applied anywhere later would leave one of them describing
	// the pre-elision fixture. Applied to in.Fixture itself (a deep copy; see
	// elideSkillSources) so there is exactly one fixture in scope from here on
	// and no call site can pick the wrong one.
	elidedFixture, elisions, err := elideSkillSources(in.Fixture, in.ElideSkills)
	if err != nil {
		return Result{}, nil, err
	}
	in.Fixture = elidedFixture

	prefixes, declared := declaredTools(in.Fixture)

	// The sandbox half is kept SEPARATE from the MCP prefixes rather than merged
	// into them, even though both split an LLM-facing name the same way: the
	// fold has to invert a sandbox result's composition and hand an MCP result
	// back verbatim, so a merged list would make it treat one as the other.
	sandbox := sandboxTools(in.Fixture)
	sandboxPrefixes := slices.Sorted(maps.Keys(sandbox))

	folded, err := Fold(recs, FoldOptions{MCPPrefixes: prefixes, SandboxPrefixes: sandboxPrefixes})
	if err != nil {
		return Result{}, nil, err
	}

	// A trigger-started session's transcript turn 0 is the prompt the runner
	// SYNTHESIZED from the delivery, not something a person typed. Fold cannot
	// know that — it never reads Records.Trigger — so the suppression happens
	// here, where both halves are in hand.
	//
	// Only the userTurn goes. The step's Expect stays: the replayed run
	// synthesizes its own prompt from the same delivery, so the divergence
	// check still describes the request that step answers, and dropping it
	// would leave the bundle's first step asserting nothing.
	// What the run got from its trigger's PROVIDER, read back by the kind that
	// composed the text it is written in. Derived BEFORE the trigger is built,
	// because the head commit is what fills bt.Trigger.HeadSHA — the value that
	// lets the replay's stand-in answer the pull-request read the status surface
	// makes before it does anything else.
	providerState := deriveProviderState(recs, providerSurfaceTools(in.FixtureTools), in.TriggerProvider)

	trigger := triggerFor(recs, in.Fixture.TriggerChannel, providerState)
	if trigger != nil && len(folded.UserTurns) > 0 {
		folded.UserTurns = folded.UserTurns[1:]
	}

	rewritten, err := RewriteFixture(in.Fixture)
	if err != nil {
		return Result{}, nil, err
	}
	files := rewritten.Files

	seed, err := DeriveSeed(recs, in.Expand, in.SessionGrants)
	if err != nil {
		return Result{}, nil, err
	}
	if len(seed.Tuples) > 0 {
		doc, err := bootstrapDoc(seed.Tuples)
		if err != nil {
			return Result{}, nil, err
		}
		// Appended BEFORE the self-check so the leak scan sees these bytes too:
		// the seed carries subject ids read off a live cluster, and this file is
		// written into a repo like every other.
		files = append(files, FixtureFile{Name: bootstrapFileName, YAML: doc})
	}

	// The exemption exists only alongside the claim. A session recorded before
	// the tool_catalog Kind shipped pins no catalogs, so the replay compares no
	// tool sets and there is nothing for an ungated tool to be excused FROM —
	// carrying the list anyway would read as a weakening that is not there, and
	// Bundle.Validate refuses that shape outright. Nothing is lost: with no
	// claim, the replay offers whatever the fixture composes, as it always did.
	expectedExtras := rewritten.UngatedTools
	if len(folded.ToolCatalogs) == 0 {
		expectedExtras = nil
	}

	autoApprove, autoDeny, approveAs, undetermined := DeriveApprovalSettings(recs)

	// Every DECLARED tool the transcript never called gets a placeholder entry,
	// the same one a human writes by hand into an authored bundle. Done HERE
	// rather than in Fold because it is the assembly step's knowledge: only
	// this function holds both the transcript and the fixture's allowlist.
	// Before the self-check, so the bundle and the findings describe one map.
	placeholders := addPlaceholderToolOutputs(folded, declared)

	bundle := bt.Bundle{
		Name:        in.Name,
		Description: in.Description,
		// The bundle's own directory: WriteResult puts the fixture manifests
		// beside bundle.json, and the harness applies every *.yaml it finds
		// there. A capture cannot know where a person will file it, so it names
		// the conventional home every existing scenario already uses.
		AgentDir:   filepath.ToSlash(filepath.Join("testdata", in.Name)),
		AgentClass: in.Fixture.Class.Name,
		// Named only when the fixture emitted a UserIdentity for it — that CR
		// is addressed by this user's canonical subject, so the two must agree
		// or the replayed session resolves an empty catalog and parks. Taken
		// from the rewrite that derived the name rather than restated here.
		DefaultUser:        rewritten.DefaultUser,
		ToolOutputs:        emptyToNil(folded.ToolOutputs),
		ToolOutputSequence: emptyToNilSlices(folded.ToolOutputSequence),
		ToolErrors:         folded.ToolErrors,
		// The replies the capture had to CAN rather than let run. Empty for
		// every session that touched no cannable meta tool, which is most of
		// them; an entry is an admission of lost coverage and raises a warning
		// naming exactly which code stops running. See bt.MetaToolReplies.
		MetaToolReplies: folded.MetaToolReplies,
		Capture: &bt.Capture{
			Session:     in.Session,
			Cluster:     in.Cluster,
			CapturedAt:  in.Now,
			ServedModel: folded.ServedModel,
			OapVersion:  in.OapVersion,
			// Set here rather than between emit's two passes, where the
			// redaction record has to be set: these counts are known before a
			// byte is marshalled, because the rewrite that produced them ran
			// over the manifests rather than over the output.
			SkillElisions: elisions,
		},
		// The ids the run minted, for the replay's own minters to hand back. The
		// recorded arguments above keep their literal values, so this is what
		// makes them address the same operations and artifacts a second time.
		MintedIDs: folded.MintedIDs,
		// What the run was OFFERED at each turn, and the tools the fixture
		// rewrite let through early. The two belong together: the catalogs are
		// the claim, and this is the only exemption from it — derived from what
		// RewriteFixture actually dropped, never restated here, so the
		// exemption cannot outlive the rewrite that earned it.
		ToolCatalogs:       folded.ToolCatalogs,
		ExpectedExtraTools: expectedExtras,
		UserTurns:          userTurns(folded.UserTurns),
		Trigger:            trigger,
		// The provider state the replay has to seed into its stand-ins. Nil
		// when the run touched no provider, which is most sessions. See
		// bt.StandIn: every value in it is read back from a record by the party
		// that owns its format, never invented, because a stand-in seeded with
		// something nobody observed replays cleanly and proves nothing.
		StandIn: standInFor(providerState, in.MentionStandIn),
		// The capability half of the same rewrite. It lives in its own top-level
		// field rather than inside StandIn because it predates this work and
		// every hand-authored bundle that delivers an artifact already sets it;
		// a capture reaching for the same switch keeps one spelling.
		FakeDeliverySurfaces: in.FakeDeliverySurfaces,
		AutoApprove:          autoApprove,
		AutoDeny:             autoDeny,
		AutoApproveAs:        approveAs,
		LLM:                  folded.LLM,
		Assert:               DeriveAssertions(recs, folded),
	}

	var payload []byte
	if trigger != nil {
		payload = recs.Trigger.Body
	}

	// Every byte that will be written, assembled BEFORE the self-check and with
	// the redactions already applied.
	//
	// The order is what makes a redaction unable to hide a structural finding,
	// and it is self-enforcing rather than a rule someone has to remember.
	// Redact first, scan the result: if a rule genuinely removed a credential
	// the scan has nothing left to find, and if it did not, the scan still
	// fires over the same bytes that reach the repo. There is deliberately no
	// flag on the other side of this — no --no-secret-scan, no severity
	// override — because the only way to make a structural finding go away is
	// to make the thing it found go away.
	res, err := emit(bundle, files, GoldenTrace(recs), payload, in.Redact)
	if err != nil {
		return Result{}, nil, err
	}

	findings := SelfCheck(SelfCheckInput{
		Records: recs,
		Folded:  folded,
		Seed:    seed,
		// Post-redaction, so the Secret-bearing gate reads the same bytes the
		// scans do rather than a second version of the fixture nobody writes.
		Files: res.Fixture,
		// Every emitted surface, final bytes. See Result.Emitted.
		Emitted: res.Emitted,
		// The assembled bundle, so the self-check can run the replay driver's
		// OWN load preconditions over the exact value WriteResult will marshal.
		// A capture that reports clean and writes a file the loader refuses is
		// the failure this closes.
		Bundle:        res.Bundle,
		DeclaredTools: declared,
		SandboxTools:  sandbox,
		MetaTools:     in.MetaTools,
		// What the LIVE run was offered and what the REWRITTEN fixture will
		// offer, side by side. The gap between them is a tool the rewrite
		// destroyed, and it is invisible in either list alone.
		FixtureTools: in.FixtureTools,
		LiveSecrets:  in.LiveSecrets,
		// From the rewrite that emitted them, so which placeholders stand in
		// for live material and which are invented cannot be re-derived — and
		// so cannot disagree. See RewriteResult.ShadowedSecrets.
		ShadowedSecrets: rewritten.ShadowedSecrets,
		// Read from the class the fixture is built out of, never passed in
		// separately: it is the single fact that tells an empty
		// TransformedToolUseIDs meaning "no guard rewrote anything" from one
		// meaning "nobody gathered", and a caller-supplied copy could disagree
		// with the manifests in the same Result.
		GuardConfigured: in.Fixture.Class.Spec.ToolGuard != nil,
		// Same class, same reason: read from the manifests the fixture is
		// built out of, so the refusal cannot disagree with what was emitted.
		ClassSkills: classSkillRefs(in.Fixture.Class),
		// From the rewrite that wrote them, never re-derived from the class the
		// line above reads: a check comparing the class to itself can never see
		// a gap. See SelfCheckInput.CapturedSkills.
		CapturedSkills:      rewritten.EmittedSkills,
		SkillsWithoutBundle: rewritten.SkillsWithoutBundle,
		// What the elision actually did, from the rewrite that did it — never
		// re-derived from in.ElideSkills, which is only what was ASKED for. A
		// rule that matched nothing is already a refusal, but a check reading
		// the request rather than the result could not tell a rewrite that
		// stopped eliding from one that never had anything to elide.
		SkillElisions: elisions,
		// From the rewrite that emitted the credentials, for the reason above:
		// a check re-deriving this from the live AgentIdentity could not see a
		// rewrite that started dropping or converting one.
		MintedCredentials: rewritten.MintedCredentials,
		// Stated rather than inferred from an empty provider seed: only the
		// caller can tell "this kind reports no trigger status" from "nobody
		// resolved the reporter". See SelfCheckInput.TriggerProviderMissing.
		TriggerProviderMissing: in.TriggerProvider == nil,
		TriggerChannel:         in.Fixture.TriggerChannel,
		UndeterminedApprovals:  undetermined,
		PlaceholderTools:       placeholders,
	})

	return res, findings, nil
}

// userTurns lifts the folded human turns into the bundle's turn type.
//
// Text and nothing else, because text is all Fold recovers. A bundle turn can
// also carry an ATTACHMENT, and a capture never emits one: reconstructing it
// would mean writing the uploaded bytes back out as a fixture file, which
// nothing here does. A captured turn that was only an attachment is dropped
// and reported instead — see the attachment-unrepresentable note in
// selfcheck.go, which surfaces it as CodeUnmappedTurn.
func userTurns(in []string) []bt.UserTurn {
	if len(in) == 0 {
		return nil
	}
	out := make([]bt.UserTurn, 0, len(in))
	for _, text := range in {
		out = append(out, bt.UserTurn{Text: text})
	}
	return out
}

// classSkillRefs is the canonical names the class opts into. Nil for a class
// with no skills and for a nil class, so the check it feeds says nothing about
// either. See SelfCheckInput.ClassSkills.
func classSkillRefs(class *spiceboxv1alpha1.AgentClass) []string {
	if class == nil {
		return nil
	}
	out := make([]string, 0, len(class.Spec.Skills))
	for _, s := range class.Spec.Skills {
		out = append(out, s.Ref)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// emit builds every file a capture writes, applies the redactions to the FINAL
// bytes, and records on the bundle what those redactions replaced.
//
// TWO marshalling passes, and the second is not redundant. The hit counts
// belong in the bundle (bt.Capture.Redactions), so they must be known before
// the bundle is marshalled — and they can only be counted over marshalled
// bytes. Pass one counts; pass two marshals the bundle carrying those counts
// and redacts for real.
//
// The passes are then required to AGREE. A disagreement means a rule's original
// matched something inside the redaction record this function just added — an
// operator redacting the word "count", say — so the number the bundle states is
// not the number of occurrences it removed. Refusing names the rule by its
// replacement token; accepting would ship a bundle whose own provenance lies
// about how much of it was rewritten.
func emit(bundle bt.Bundle, files []FixtureFile, golden, payload []byte, rules []Redaction) (Result, error) {
	if len(rules) == 0 {
		return assemble(bundle, files, golden, payload, nil)
	}
	if bundle.Capture == nil {
		return Result{}, fmt.Errorf("steelthread: emit: %d redaction(s) were supplied but the bundle carries no "+
			"capture stanza to record them on; the emitted files would be rewritten with nothing saying so",
			len(rules))
	}
	// Fail closed on an unresolved rule. Capture resolves every rule before it
	// gets here, so reaching this means a new call path skipped that step — and
	// applyRedactions would DELETE the original rather than replace it, leaving
	// a bundle with a hole where a stand-in should be and a record claiming a
	// token that is nowhere in the files.
	for _, r := range rules {
		if r.New == "" {
			return Result{}, fmt.Errorf("steelthread: emit: a redaction reached the emitter with no " +
				"replacement; call ResolveRedactions before emitting, or applying it would delete the " +
				"value instead of standing in for it")
		}
	}

	first, err := assemble(bundle, files, golden, payload, rules)
	if err != nil {
		return Result{}, err
	}

	bundle.Capture.Redactions = redactionRecord(rules, first.redactionCounts)
	second, err := assemble(bundle, files, golden, payload, rules)
	if err != nil {
		return Result{}, err
	}
	for i, r := range rules {
		if first.redactionCounts[i] == second.redactionCounts[i] {
			continue
		}
		return Result{}, fmt.Errorf("steelthread: the redaction replacing with %q matched %d time(s) before its "+
			"own record was added to the bundle and %d time(s) after, so its original collides with the "+
			"redaction record itself and the count the bundle would state is wrong. Choose an original that "+
			"does not appear in a bundle's capture stanza",
			r.New, first.redactionCounts[i], second.redactionCounts[i])
	}
	return second, nil
}

// assemble marshals the bundle, redacts every surface, and returns the complete
// emission plus the per-rule hit counts summed across all of them.
//
// The surfaces are appended in WRITE order, and bundle.json goes down first
// because it is the marker both a reader and the suite identify a capture by.
func assemble(bundle bt.Bundle, files []FixtureFile, golden, payload []byte, rules []Redaction) (Result, error) {
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("steelthread: marshal bundle: %w", err)
	}
	// A trailing newline: without one every later edit shows the last line as
	// changed, forever.
	raw = append(raw, '\n')

	res := Result{Bundle: bundle, redactionCounts: make([]int, len(rules))}
	add := func(name string, data []byte) []byte {
		out, counts := applyRedactions(data, rules)
		for i, n := range counts {
			res.redactionCounts[i] += n
		}
		res.Emitted = append(res.Emitted, EmittedFile{Name: name, Bytes: out})
		return out
	}

	add(bundleFileName, raw)
	res.Golden = add(goldenFileName, golden)
	for _, f := range files {
		res.Fixture = append(res.Fixture, FixtureFile{Name: f.Name, YAML: add(f.Name, f.YAML)})
	}
	if bundle.Trigger != nil {
		// path, not filepath: EmittedFile.Name is slash-separated on every
		// platform, and WriteResult is what turns it into a local path.
		res.TriggerPayload = add(path.Join(payloadsDir, bundle.Trigger.Payload), payload)
	}
	return res, nil
}

// declaredTools reads the session's MCP wiring twice over, because the fold and
// the self-check need it in two different shapes and neither should ask a
// caller to supply what the manifests already say.
//
// prefixes are the LLM-facing prefixes the runner synthesizes "<prefix>_<tool>"
// from. declared maps each prefix to the SERVER-side names its allowlist
// declares — the names h.MCP.OnTool is keyed by, and therefore the names a
// toolOutputs entry has to use.
//
// TWO sources feed this, and both are MCP at the dispatch layer:
//
//   - The class's spec.mcpServers[]. Prefix is the ref's `name`; the tool names
//     are resolved through `ref` to the MCPServer CR's spec.tools rather than
//     assumed to equal the prefix, because they differ whenever a class names a
//     server something other than its CR name.
//   - The session's status.resolvedSidecarToolboxes[]. Prefix is the resolved
//     entry's `Name` and the tool names are its `Spec.Tools[].Name`. This is not
//     an approximation of the sidecar case: sidecartoolbox.Synthesize builds a
//     synthetic MCPServer with ObjectMeta.Name = rt.Name and Spec.Tools =
//     rt.Spec.Tools and hands it to the SAME mcptool.Synthesize the remote path
//     uses, so a sidecar tool is indistinguishable from a remote MCP tool by the
//     time it reaches the model — and at replay it is served by the same single
//     fake MCP stub.
//
// Reading sidecars from FixtureInput.SidecarToolboxes (the resolved status)
// rather than in.Class.Spec.SidecarToolboxes is deliberate; see that field's doc.
// Omitting them entirely — which this function did until sidecar support
// landed — made every sidecar call fall through to "not an MCP server" and
// raised a spurious hard CodeSandboxToolCall for each one.
func declaredTools(in FixtureInput) (prefixes []string, declared map[string][]string) {
	byName := make(map[string]*spiceboxv1alpha1.MCPServer, len(in.MCPServers))
	for _, s := range in.MCPServers {
		if s != nil {
			byName[s.Name] = s
		}
	}
	declared = map[string][]string{}
	for _, ref := range in.Class.Spec.MCPServers {
		prefixes = append(prefixes, ref.Name)
		var tools []string
		if server := byName[ref.Ref]; server != nil {
			for _, tl := range server.Spec.Tools {
				tools = append(tools, tl.Name)
			}
		}
		declared[ref.Name] = tools
	}
	for _, rt := range in.SidecarToolboxes {
		if rt.Name == "" {
			continue
		}
		prefixes = append(prefixes, rt.Name)
		var tools []string
		for _, tl := range rt.Spec.Tools {
			tools = append(tools, tl.Name)
		}
		declared[rt.Name] = tools
	}
	return prefixes, declared
}

// triggerFor builds the bundle's trigger block from the recorded delivery.
//
// Nil unless BOTH halves are present: a delivery record with no input Channel
// named has no fixture Channel for the driver to sign with, and a named Channel
// with no record has no body to sign. Either alone is reported by the
// self-check (CodeNoTriggerRecord) rather than papered over with a
// half-populated trigger the loader would reject.
//
// HeadSHA is DERIVED, and is the one field here that is not simply copied off
// the delivery record. It is the revision the status surface resolved by
// READING the triggering resource, so it exists in no record of the delivery
// itself — only inside text the channel kind composed, which is where
// deriveProviderState reads it from. Empty when nothing could be read, and the
// self-check refuses that for a run that called the surface: the stand-in
// answers the pull-request read with nothing and the surface fails outright
// with "response carried no head commit".
//
// StatusName is still left EMPTY. It is read only by assert.triggerStatus, and
// what a fixture SHOULD report back is a claim about what the scenario is FOR —
// the same judgement DeriveAssertions declines to guess for
// SystemPromptContains.
func triggerFor(recs Records, channel string, provider channelkinds.TriggerProviderState) *bt.Trigger {
	if recs.Trigger == nil || channel == "" {
		return nil
	}
	return &bt.Trigger{
		Channel:    channel,
		Payload:    triggerPayloadFileName,
		Event:      recs.Trigger.Event,
		ChannelKey: recs.Trigger.ChannelKey,
		HeadSHA:    provider.SurfaceRevision,
	}
}

// providerSurfaceTools is the set of tool names whose results reached the
// trigger's own provider, taken off the caller's fixture-tool prediction.
//
// Read from the SAME list the fixture-tool checks read rather than recomputed:
// a second derivation of "which tools reach the provider" could disagree with
// the one the findings are written against, and the disagreement would show up
// as a provider id filed under a tool that never talked to one.
func providerSurfaceTools(tools []FixtureTool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		if t.ExternalSurface {
			out[t.Name] = true
		}
	}
	return out
}

// standInFor assembles the bundle's stand-in block, or nil when there is
// nothing to seed.
//
// Nil rather than an empty struct so a bundle that touched no provider and one
// built by hand in a test are the same value — the same discipline
// emptyToNil applies to the tool maps.
func standInFor(provider channelkinds.TriggerProviderState, mentions *bt.MentionStandIn) *bt.StandIn {
	if len(provider.MintedIDs) == 0 && mentions == nil {
		return nil
	}
	return &bt.StandIn{
		TriggerStatusIDs: provider.MintedIDs,
		Mentions:         mentions,
	}
}

// bootstrapDoc renders the derived seed as the SpiceDBBootstrap CR the harness
// already applies — the same declarative shape a hand-authored fixture uses, so
// a captured scenario and an authored one seed by the same route.
//
// Canonicalize is left FALSE. A recorded subject id is already canonical (a
// base64url digest, never an email), and asking the controller to canonicalize
// it again would rewrite the id the run's own Checks were answered for.
func bootstrapDoc(tuples []Tuple) ([]byte, error) {
	bs := &spiceboxv1alpha1.SpiceDBBootstrap{
		TypeMeta:   typeMeta("SpiceDBBootstrap"),
		ObjectMeta: fixtureMeta(bootstrapName),
	}
	for _, t := range tuples {
		resType, resID, ok := strings.Cut(t.Resource, ":")
		if !ok || resType == "" || resID == "" {
			return nil, fmt.Errorf("steelthread: seed tuple resource %q is not \"type:id\"", t.Resource)
		}
		subject, relation, _ := strings.Cut(t.Subject, "#")
		subType, subID, ok := strings.Cut(subject, ":")
		if !ok || subType == "" || subID == "" {
			return nil, fmt.Errorf("steelthread: seed tuple subject %q is not \"type:id[#relation]\"", t.Subject)
		}
		ref := spiceboxv1alpha1.SpiceDBSubjectRef{Type: subType, Relation: relation}
		if subID == wildcardSubjectID {
			ref.Wildcard = true
		} else {
			ref.ID = subID
		}
		bs.Spec.Relationships = append(bs.Spec.Relationships, spiceboxv1alpha1.SpiceDBBootstrapRelationship{
			Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: resType, ID: resID},
			Relation: t.Relation,
			Subject:  ref,
		})
	}
	return marshalDocs([]any{bs})
}

// emptyToNil / emptyToNilSlices drop a map the fold allocated but never filled.
// bt.Bundle marshals both with omitempty, and an allocated-but-empty map still
// serializes as {} — a key in the emitted JSON that says nothing, differing
// from an authored bundle for no reason a reader could act on.
func emptyToNil(m map[string]json.RawMessage) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return m
}

func emptyToNilSlices(m map[string][]json.RawMessage) map[string][]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	return m
}

// placeholderToolOutput is what a declared tool with no OBSERVED result gets.
//
// An EMPTY OBJECT, deliberately, and not something that looks like data. The
// shipped authored bundles write things like {"results": []} into the same
// slot, but a capture has no business implying a shape it never observed:
// nothing is known about what this tool returns. {} is honestly nothing.
var placeholderToolOutput = json.RawMessage("{}")

// addPlaceholderToolOutputs gives every DECLARED tool with no observed result
// an entry, and returns their names.
//
// bt.Bundle.ToolOutputs' own doc says why one is needed: every tool the
// MCPServer declares needs an entry, because the fixture's allowlist is
// validated at class admission and a missing handler surfaces as
// AgentClassMCPServerInvalid/AllowlistDrift — an error that never names the
// tool. A human writing a bundle satisfies that by hand. The capture used to
// REFUSE instead, and that one rule disqualified most real sessions: an MCP
// server exposes a catalogue and any one session touches part of it, so "the
// transcript called every tool its server declares" is the exception, not the
// rule.
//
// # The value is never served, in each of the three cases
//
// Which is what makes synthesizing one faithful rather than a fudge, and why
// the reader is told with a warning instead of the capture being refused.
//
//   - The transcript never called the tool, so the replay never calls it
//     either and the handler is never invoked. Its only job is to exist.
//   - The transcript's only calls were GATE REFUSALS. The replay's own gate
//     refuses them again — a denied tool never reaches the MCP server — so the
//     handler is never invoked here either.
//   - The transcript's only calls were UPSTREAM errors. The tool has a
//     toolErrors entry, and MCPStub.OnToolError wins over OnTool for the same
//     name, so the error is served and the placeholder is not. It has to exist
//     anyway: the stub builds tools/list from its OnTool registrations alone,
//     so a tool registered ONLY as an error is missing from that list and the
//     class fails admission on the very drift this function exists to prevent.
//
// The refused case used to be EXCLUDED here, on the ground that {} would make
// the replay serve a success where the run was denied. That reasoning held only
// while a refusal was indistinguishable from an upstream failure; now the first
// never reaches the stub and the second is answered by its own registration.
//
// Sorted, so a re-capture of one session is byte-identical.
func addPlaceholderToolOutputs(folded Folded, declared map[string][]string) []string {
	var added []string
	for _, tools := range declared {
		for _, tool := range tools {
			if _, ok := folded.ToolOutputs[tool]; ok {
				continue
			}
			if _, ok := folded.ToolOutputSequence[tool]; ok {
				continue
			}
			if slices.Contains(added, tool) {
				continue
			}
			added = append(added, tool)
		}
	}
	slices.Sort(added)
	for _, tool := range added {
		// Mutates the caller's map, which is the point: Folded holds a
		// reference, and Capture hands the same one to the bundle and to the
		// self-check — so the two cannot disagree about what was emitted.
		folded.ToolOutputs[tool] = placeholderToolOutput
	}
	return added
}

// WriteOption modifies WriteResult.
type WriteOption func(*writeOptions)

type writeOptions struct{ overwrite bool }

// Overwrite makes WriteResult REPLACE what is in the directory rather than
// refusing it: the directory's existing contents are removed before the new
// capture is written. See WriteResult on why replacing, not merging, is what
// the option has to mean.
func Overwrite() WriteOption {
	return func(o *writeOptions) { o.overwrite = true }
}

// WriteResult writes a capture to dir.
//
// It writes r.Emitted and NOTHING else — the same list the self-check's secret
// scans ran over, byte for byte. That identity is the point: while the writer
// re-derived its own bytes from r.Bundle, the scans could look at one thing and
// the repo receive another, which is exactly how bundle.json and the trigger
// payload shipped unscanned. A Result carrying no Emitted is refused rather
// than written empty; see the emptiness check below.
//
// A NON-EMPTY dir is refused unless Overwrite() is passed, and with it the
// directory is EMPTIED first. Both halves guard the same failure from opposite
// sides, and the second is not a formality:
//
//   - Refusing an unexpected non-empty directory catches a mistyped output
//     path, which would otherwise leave one capture's bundle.json beside
//     another's fixture — a scenario that is neither, with nothing about the
//     resulting replay failure pointing back at the typo.
//   - Emptying on overwrite catches the same thing arriving by the front door.
//     Re-capturing a session whose class has since dropped an MCPServer leaves
//     the old 02-mcpserver.yaml in place, and the harness applies EVERY *.yaml
//     it finds in an agentDir (test/e2e's applyAgentDir), so the replay would
//     boot a server the captured session never had. A stale payloads/ body
//     survives the same way. A partial overwrite is exactly the merge the
//     refusal above exists to prevent.
//
// # What Overwrite() is allowed to delete
//
// A directory ALREADY HOLDING a bundle.json, and nothing else. Replacing a
// previous capture is the whole use case, and that file is what identifies one:
// it is the marker the bronzethread suite itself discovers a scenario by.
//
// The bound is the point, because without it the flag recursively deletes an
// arbitrary user-named directory. `--output .` from a repo root, or one
// mistyped word naming a testdata PARENT instead of the bundle inside it, would
// empty it — and a directory that is not a capture is precisely the typo the
// emptiness refusal above exists to catch, arriving with a flag attached. This
// repo's rule is to remove by name and never sweep state we did not create;
// requiring the marker is how that rule is kept while still replacing whole.
//
// Both refusals happen before the first byte is written or removed, so a
// refused call leaves the directory exactly as it found it. The removal is
// scoped to dir's own entries — never anything above it.
func WriteResult(dir string, r Result, opts ...WriteOption) error {
	var o writeOptions
	for _, opt := range opts {
		opt(&o)
	}

	// Fail closed on a Result that was not built by Capture. Emitted is the
	// only thing this function writes, so an empty one would create the
	// directory, report success, and leave nothing in it — and with Overwrite
	// it would first DELETE a real capture to put nothing in its place. The
	// bundle.json requirement is the same marker holdsACapture uses: a write
	// that does not produce one produces a directory the suite silently skips.
	if !slices.ContainsFunc(r.Emitted, func(f EmittedFile) bool { return f.Name == bundleFileName }) {
		return fmt.Errorf("steelthread: refusing to write %s: the Result carries %d emitted file(s) and no %s, "+
			"so it did not come from Capture. WriteResult writes Result.Emitted and nothing else",
			dir, len(r.Emitted), bundleFileName)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("steelthread: create %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("steelthread: read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		if !o.overwrite {
			return fmt.Errorf("steelthread: %s is not empty (%d entries); pass the overwrite option to replace it, "+
				"or choose a directory of its own — merging two captures produces a scenario that is neither",
				dir, len(entries))
		}
		if !holdsACapture(entries) {
			return fmt.Errorf("steelthread: refusing to replace %s: it is not empty (%d entries) and holds no %s, "+
				"so it is not a capture. Overwriting replaces a PREVIOUS capture whole, which means deleting "+
				"what is there; a directory without that file is the mistyped path the non-overwrite refusal "+
				"catches, and nothing has been removed. Point --output at the bundle directory itself, or at "+
				"an empty one",
				dir, len(entries), bundleFileName)
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return fmt.Errorf("steelthread: replace %s: remove stale %s: %w", dir, e.Name(), err)
			}
		}
	}

	for _, f := range r.Emitted {
		local := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return fmt.Errorf("steelthread: create the directory for %s: %w", f.Name, err)
		}
		if err := os.WriteFile(local, f.Bytes, 0o644); err != nil {
			return fmt.Errorf("steelthread: write %s: %w", f.Name, err)
		}
	}
	return nil
}

// holdsACapture reports whether dir's entries carry the marker that identifies
// a previous capture: bundle.json, the same file the bronzethread suite
// discovers a scenario by.
//
// Deliberately the ONE file, and deliberately not a deep inspection. Anything
// smarter — parsing the bundle, checking its name matches — would refuse to
// replace a capture whose earlier write was interrupted, which is exactly when
// somebody reaches for the flag. The question this answers is only "did we make
// this directory?", and that is the question that bounds the delete.
//
// The name comparison alone is what rejects a directory of OTHER captures: a
// testdata parent holds entries called scenario-one, agent-reviewbot and the
// like, none of which is named bundle.json, so none of them qualifies it.
// !e.IsDir() covers the one case the name check cannot — an entry that IS named
// bundle.json and is a DIRECTORY. Narrow, and load-bearing anyway: without it
// such a directory would qualify its parent as a capture and the RemoveAll loop
// would run over something nothing here wrote.
func holdsACapture(entries []os.DirEntry) bool {
	for _, e := range entries {
		if !e.IsDir() && e.Name() == bundleFileName {
			return true
		}
	}
	return false
}

// FormatFindings renders findings for a terminal, hard ones first, so a reader
// sees what stops the capture before what merely qualifies it. Returns the
// number of hard findings alongside the text, because the caller's next line is
// always about that count.
//
// Every finding is printed, never the first one only: fixing five problems one
// command invocation at a time, against a live cluster, is the slow way to
// learn about five problems.
func FormatFindings(findings []Finding) (text string, hard int) {
	if len(findings) == 0 {
		return "", 0
	}
	ordered := slices.Clone(findings)
	slices.SortStableFunc(ordered, func(a, b Finding) int { return int(a.Severity) - int(b.Severity) })

	var sb strings.Builder
	for _, f := range ordered {
		if f.Severity == SeverityHard {
			hard++
		}
		fmt.Fprintf(&sb, "%-5s %-30s %s\n", f.Severity.String(), f.Code, f.Message)
	}
	return sb.String(), hard
}
