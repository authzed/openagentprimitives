package steelthread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Severity ranks a Finding. SeverityHard is the zero value on purpose: a
// Finding constructed without one reads as "refuse", never as "mention it and
// carry on", so the failure mode of forgetting to set the field is a capture
// that does not ship rather than a bundle that silently replays wrong.
type Severity int

const (
	// SeverityHard stops the capture. The bundle it would emit replays
	// differently than it recorded, and the divergence would surface minutes
	// away as a failure in some suite, pointing at the replay instead of here.
	SeverityHard Severity = iota
	// SeverityWarn is emitted alongside the bundle. The capture is less
	// evidentiary than it could be, but what it does claim is still true.
	SeverityWarn
)

func (s Severity) String() string {
	if s == SeverityWarn {
		return "warn"
	}
	return "hard"
}

// The finding codes, one per way a capture can look fine and replay wrong.
//
// # What these checks deliberately do NOT catch
//
// Every entry below is a real fidelity gap. All but the last have no clean
// mechanical detection, so a human reading the emitted bundle is the only
// remaining check; the last one IS detected, and is listed here anyway because
// what it reports cannot be fixed in this package. They are recorded rather
// than left for the next person to rediscover.
//
//   - model-changed-mid-session. Folded.ServedModel is last-wins, so a session
//     that was served by two models names only the last one. Detecting it would
//     mean deciding which model the bundle is evidence FOR, which is a
//     judgement, not a fact in the transcript.
//
//   - RESOLVED, kept as the worked example: unreproduced-render-handle. This
//     entry used to say that a render HANDLE (the ArtifactRender CR's own
//     `ar-<session>-<suffix>` name) and a REVISION id (`artrev-…`) were beyond
//     reach — the first minted from a random suffix at CR-create time, the
//     second derived from the UID the API server assigns — and that reproducing
//     them would mean overriding the derivation that makes prepare→await
//     idempotent. The first half was simply a missing seam
//     (artifacts.WithRenderNameMinter). The second half was the interesting
//     one, and it was wrong about the conclusion rather than about the risk: a
//     seam that drew a fresh id per CALL really would break idempotence, so the
//     seam takes the UID as a KEY and the replay memoizes on it
//     (bt.MintedIDSequence.KeyedMinter). The derivation's property — same
//     revision, same id, however many times you ask — is preserved rather than
//     worked around. Both are families now; an unpinned one is refused by
//     unreproducible-id.
//
//   - narration-duplicated. The replay driver's RespondToUser emits its own
//     text block AND the tool call, so a captured [BareText(x), Text(y)]
//     reconstructs as text, text, tool_use where the original run had
//     text, tool_use. The extra block changes the reconstructed history, not
//     the assertions, and no field distinguishes the two shapes at capture
//     time.
//
//   - endturn-never-emitted. A captured assistant turn with stop_reason
//     end_turn folds to text-only parts; bt.ReplyPart.EndTurn is never set,
//     because memory.Turn records no stop reason to read it from.
//
//   - allowlist-drift-from-the-other-side. A DECLARED tool with no recorded
//     output is HANDLED, not caught: the assembly step synthesizes a
//     placeholder entry and CodePlaceholderToolOutputs says so. Its mirror — a
//     recorded output for a tool the fixture's allowlist will not declare —
//     fails at replay by the same AllowlistDrift route, from the other
//     direction, and nothing here catches it. Reachable when the catalog moved
//     mid-session, which is why Records.ToolCatalogs exists to be consulted.
//     The same defect has a second face: a prefix present in DeclaredTools with
//     an EMPTY tool list passes every gate, because mcp-name-collision iterates
//     that list while checkToolCalls still resolves the prefix. Both are the
//     round-2 lesson (gate on what is SCANNABLE, not on what is present)
//     applied to LiveSecrets and TransformedToolUseIDs but not to
//     DeclaredTools. Deliberately left as a known omission rather than
//     half-closed; closing it means deciding what the fixture's allowlist WILL
//     say, which is the assembly step's knowledge, not this file's.
//
//   - cannable-meta-erred-every-call. A cannable meta tool (bt.MetaToolCannable)
//     whose calls ALL returned an error cans nothing, by the rule that an error
//     result is never canned in either direction — a gate refusal because
//     canning it would mask a gate that stopped refusing, and the tool's own
//     refusal because our code composes it again at replay. That is right for
//     the second case and only mostly right for the first: "memory not
//     available" is a platform string the replay's own memory-backed fixture
//     will not reproduce, and the step's expectation was derived from it. No
//     bundle field could express it — the format has no error half, deliberately
//     — so there is nothing for a finding to tell a reader to do. Reachable only
//     from a session whose memory was down, and visible as an ordinary step
//     failure if it ever is.
//
//   - attachment-unrepresentable. A user turn whose only block is an
//     attachment has no bundle representation at all. Surfaces as
//     CodeUnmappedTurn, which is adequate: the reader is told the turn was
//     dropped, which is the fact that matters.
//
//   - mid-turn-arrival-timing. A capture cannot express that a message
//     ARRIVED while the agent was mid-turn rather than after it finished —
//     only WHAT the message said, not WHEN channelsd delivered it relative to
//     the model's own work. That timing is a channelsd/runner plumbing fact
//     (drainInbox places every queued "inbox" turn at a yield boundary — see
//     Fold's inbox/inbox_done case — never mid-Send), and the transcript
//     replay contract this package captures does not reach it: there is no
//     field a capture could set and no bronze bundle, authored or captured,
//     that exercises the timing either. Nothing to detect and nothing a
//     capture could do differently, so it gets no finding.
//
//   - trigger-turn-promoted. A trigger-started session's index-0 turn is the
//     SYNTHESIZED trigger prompt, and Fold promotes it to UserTurns[0] — Fold
//     never reads Records.Trigger, so it cannot know. TASK 12 SUPPRESSES THIS
//     at the assembly step, where both the trigger record and the folded turns
//     are in hand; do not add a check for it here, because by the time the
//     bundle is assembled correctly there is nothing left to find.
//
//   - tool-error-not-uniform. THE ONE ENTRY HERE THAT IS DETECTED
//     (CodeToolErrorNotUniform), because what remains is a property of the
//     stub's REGISTRATION, not of the capture. bt.Bundle.ToolErrors now says
//     "this call comes back an error", and the driver registers it through
//     MCPStub.OnToolError — but that registration is keyed by tool name and
//     wins over every canned result, so one tool cannot error on one call and
//     succeed on another. Closing THAT means a per-call error variant inside
//     toolOutputSequence, which is a feature with its own design.
//
//     Its predecessor, error-result-unrepresentable, refused any captured
//     error result at all and is gone. It conflated a GATE refusal — a call
//     that never reached the server, which the replay's own gate reproduces —
//     with an UPSTREAM failure, and in doing so refused precisely the sessions
//     this feature exists for. See gateRefused for how the two are told apart,
//     and why the classification fails closed toward "upstream".
const (
	// CodePlaceholderToolOutputs: the capture SYNTHESIZED a toolOutputs entry
	// for one or more declared tools the transcript recorded no RESULT for —
	// never called, refused by a gate before dispatch, or answered only by an
	// upstream error (which gets its own toolErrors registration and wins).
	//
	// A warning, and it replaced a hard refusal (missing-tool-output) that
	// disqualified ten of the eleven bronze scenarios measured against it. The
	// entry has to exist — the fixture's allowlist is validated at class
	// admission, and a missing handler surfaces as
	// AgentClassMCPServerInvalid/AllowlistDrift, an error that never names the
	// tool — and an authored bundle satisfies exactly the same requirement with
	// a hand-written placeholder. Refusing where a human would simply write one
	// made the capture reject the ordinary shape of a real session.
	//
	// It stays a finding rather than becoming silent because a reader of the
	// emitted bundle must not have to guess which outputs were OBSERVED and
	// which were invented. See addPlaceholderToolOutputs.
	CodePlaceholderToolOutputs = "placeholder-tool-outputs"

	// CodeUnprovenErrorOrigin: an error result on a call the authz log ALSO
	// denied, with a different message — so the capture could not prove whether
	// the platform refused the call or the upstream failed, and emitted NOTHING
	// for it: no toolErrors entry, no toolOutputs entry.
	//
	// Canning it would be worse than inaccurate; it would MASK A GATE
	// REGRESSION. Such a body is very likely the gate's own refusal text, and
	// Expect for that step was derived from those same bytes. Trace a gate that
	// stops refusing: the call now reaches the stub, the stub serves the canned
	// entry, LastToolResultContains matches the text it was derived from, and
	// the bundle PASSES with the permission boundary broken — on a scenario
	// that exists to prove the boundary holds. That is the exact defect class
	// this package was built to eliminate.
	//
	// Emitting nothing is strictly safer in both directions. A working gate
	// produces the real refusal and the Expect matches honestly; a broken one
	// reaches a tool the stub has no error registered for, cannot reproduce the
	// refusal text, and the step fails naming itself.
	//
	// A WARNING rather than a refusal, because the bundle is still faithful and
	// still worth having — the human is told an error result went unrepresented
	// and why, so they can decide whether that step still proves what they
	// wanted.
	//
	// It is reachable and not theoretical. The approval flow denies with
	// whatever BuildApprovalAsk's error said — "no one has standing to approve
	// …" — and when the ask could not be built it records nothing at all, so
	// the only per-call record left is an authz denial saying something else.
	// See gateRefused for why widening the classification to cover it would
	// stop being a proof.
	CodeUnprovenErrorOrigin = "unproven-error-origin"

	// CodeUnmappedTurn: a transcript turn the fold could not place. A turn
	// nobody mapped is a hole in the replay.
	CodeUnmappedTurn = "unmapped-turn"

	// CodeTransformedOutput: a guard rewrote a tool's output, so the recorded
	// value is post-transform and replay would transform it AGAIN. The capture
	// cannot invert that.
	CodeTransformedOutput = "transformed-output"

	// CodeToolErrorNotUniform: one MCP tool whose calls did not all fail the
	// same way — an upstream error on one call and a success on another, or two
	// upstream errors that do not say the same thing.
	//
	// The last thing a captured error result cannot express, and the cause is
	// the REGISTRATION rather than the format: bt.Bundle.ToolErrors reaches the
	// fake server through MCPStub.OnToolError, which is keyed by tool NAME,
	// wins over OnTool for every call, and is not counted. There is no per-call
	// variation to reach for, and toolOutputSequence cannot interleave with it
	// (bt.ValidateToolOutputs refuses the pairing outright).
	//
	// It replaced a far broader hard finding, error-result-unrepresentable,
	// which refused ANY captured error result — and so refused exactly the
	// sessions this feature exists for, a denied call being how an
	// authorization scenario ends. That finding conflated two different things:
	// a GATE refusal, which never reached the server and which the replay's own
	// gate reproduces from the fixture, and an UPSTREAM failure, which
	// toolErrors now expresses.
	CodeToolErrorNotUniform = "tool-error-not-uniform"

	// CodeSecretLeak: live secret material survived into an emitted file, which
	// is about to be written into a repo. The KNOWN-VALUE half of the secret
	// scan — an exact comparison against the credential values the capture read
	// while gathering the manifests.
	CodeSecretLeak = "secret-leak"

	// CodeStructuralSecret: an emitted file carries something SHAPED like a
	// credential — a PEM private key, a JWT, a provider token, a kubeconfig
	// client key, a populated token/password field.
	//
	// The half of the secret scan that catches what nobody registered.
	// CodeSecretLeak can only ever find a value the capture READ, which means a
	// value belonging to a Secret the manifests reference; the credential that
	// actually reaches a repo is the one a tool printed into its stdout, and no
	// Secret in the cluster holds it. Hard, and deliberately UNGATED: unlike
	// LiveSecrets, an empty result here honestly means "no credential shape is
	// in these bytes" rather than "nobody supplied anything to look for", so
	// there is nothing for a gate to close. See secretscan.go.
	CodeStructuralSecret = "structural-secret"

	// CodeHighEntropyBlob: an emitted file carries a long base64-alphabet run
	// whose character distribution looks like key material.
	//
	// WARN, never hard, and the concession is deliberate. An embedded image, a
	// compressed blob, a diff and a webhook body have the same statistics as a
	// key, and nothing in the bytes distinguishes them — a hard finding here
	// would refuse valid captures for a guess. A detector that refuses valid
	// work gets switched off, and a switched-off detector catches nothing at
	// all, so this one asks a human to look rather than deciding for them.
	CodeHighEntropyBlob = "high-entropy-blob"

	// CodeGuardScanSkipped: the transformed-output scan could not run. The
	// class configures a tool guard — so a guard was in a position to rewrite
	// output — but Records.TransformedToolUseIDs is empty.
	//
	// The structural twin of CodeSecretCheckSkipped, and hard for a sharper
	// reason. TransformedToolUseIDs is gathered externally from the
	// contentguard / toolguard / infoleakage audits; a gather that returns
	// nothing because the kind is absent, the scope is wrong, or a query error
	// was swallowed is indistinguishable from "no guard rewrote anything". The
	// bundle then ships with an output that replay transforms a SECOND time —
	// precisely the looks-fine-replays-wrong class this whole file exists to
	// refuse.
	CodeGuardScanSkipped = "guard-scan-skipped"

	// CodeSecretCheckSkipped: the leak scan could not run. The fixture emits a
	// placeholder Secret standing in for a LIVE one — so credential material
	// demonstrably existed and was substituted — but no value for that Secret
	// was supplied to scan for.
	//
	// Hard, and structural rather than a note for the caller to remember: this
	// is the one check whose empty input would otherwise read as a clean bill
	// of health, and whose failure mode is committing a credential to a repo.
	//
	// Gated per SECRET, not on "the fixture emits any Secret at all". The
	// coarse form charged a capture for a placeholder the rewrite INVENTED —
	// the one rewriteClass mints when a class declares no apiKey because its
	// credential comes from the settings tiers — which stands in for nothing
	// and has no live value to be read. Demanding one refused a valid capture
	// for failing to supply something that does not exist. See
	// RewriteResult.ShadowedSecrets.
	CodeSecretCheckSkipped = "secret-check-skipped"

	// CodeSkillsNotCaptured: the AgentClass opts into a skill this capture did
	// not emit a Skill CR for.
	//
	// PER REF, not per class. The capture emits a namespaced Skill together
	// with the SkillSource that owns it, so most classes are now fully
	// captured; what this code reports is the residue — a ref that resolved to
	// no namespaced Skill at all. The two ways that happens are worth telling
	// apart, and the message does: the skill is backed by a cluster-scoped
	// ClusterSkill (which nothing here emits — see gatherSkills), or the Skill
	// CR the class was validated against has since been deleted or renamed.
	//
	// Hard, because the replay cannot start. An AgentClass whose spec.skills
	// names a canonical skill no Skill or ClusterSkill in the namespace
	// carries is parked by its own reconciler at
	// Valid=False/AgentClassSkillMissing, the session reconciler refuses to
	// spawn against an invalid class, and the driver fails at its readiness
	// barrier. A warning here would let a bundle that CANNOT boot be written
	// into a repo and discovered only by whoever next ran the suite.
	CodeSkillsNotCaptured = "skills-not-captured"

	// CodeSkillBundleNotStaged: a captured Skill shipped a bundle archive —
	// the scripts and assets a sandbox-targeted skill is staged to disk WITH —
	// and the bundle emits the skill without it.
	//
	// Warn, not hard, and the asymmetry with the code above is the whole
	// point. A missing Skill stops the replay at the readiness barrier; a
	// missing bundle archive does not stop anything, because the operator's
	// own staging path treats an uncached bundle as a log-and-skip and mounts
	// the composed SKILL.md alone. So the bundle replays — it just replays a
	// skill directory holding instructions and no executables, and this
	// finding is what keeps that legible rather than silent.
	//
	// It cannot be closed by trying harder. The archive's bytes live in the
	// operator's skillbundle.Store keyed by digest, in no CR and in no durable
	// record the capture reads; there is nothing here to gather.
	CodeSkillBundleNotStaged = "skill-bundle-not-staged"

	// CodeSkillContentElided: an operator-declared skill elision rewrote a
	// third-party repo authority to a stand-in and dropped the skill content
	// that came from it. Warn, and it is the one finding here that reports
	// something the operator ASKED for rather than something the capture had to
	// work around.
	//
	// It is still a finding, for the reason CodePlaceholderToolOutputs is: a
	// reader of the emitted bundle must not have to open bundle.json to learn
	// that the Skill bodies in the manifests beside it are not what the session
	// ran against. Printing it at capture time also puts the counts in front of
	// the person who wrote the rule, which is when a rule that hit far more (or
	// far less) than they expected is cheapest to notice.
	//
	// It says nothing about safety, because the safety question is already
	// settled before this can fire: elideSkillSources REFUSES any elision of a
	// skill whose AgentClass link is not sandbox-targeted, so a bundle carrying
	// this finding is one where every elided skill reached neither the system
	// prompt nor load_skill. See SkillElision.
	CodeSkillContentElided = "skill-content-elided"

	// CodeFixtureToolsNotComputed: the caller supplied no
	// SelfCheckInput.FixtureTools, and the capture has something to compare
	// against them — a recorded tool catalog, or a recorded call to a tool that
	// is neither MCP nor sandbox.
	//
	// The gate on the two checks below, and it exists for the reason
	// CodeSecretCheckSkipped and CodeGuardScanSkipped do: an empty list has two
	// readings — "the fixture will offer nothing" and "nobody worked out what
	// the fixture will offer" — and only the second is a failure of the
	// capture. Without this gate the first check would fire on every tool the
	// run was offered, naming a cause that has nothing to do with the bundle.
	//
	// HARD, because the alternative is silence: a capture that never predicted
	// the fixture's tool set cannot claim the recorded one survives it, and
	// reporting that as clean is exactly the emit-then-fail this file exists to
	// prevent.
	CodeFixtureToolsNotComputed = "fixture-tools-not-computed"

	// CodeOfferedToolNotProducible: the recorded run was OFFERED a tool the
	// emitted fixture will not offer.
	//
	// The replay asserts the recorded catalog outright — a recorded tool
	// missing at replay is a hard failure in bt.ToolCatalogCheck — so this is
	// the same claim, made at the point a human can act on it rather than
	// minutes into a suite. Without it a bundle emits clean and dies inside
	// someone else's test run, naming the driver instead of the capture.
	//
	// The cause is general and is not about any one tool: a fixture rewrite
	// replaces a collaborator the session's tools were assembled against. The
	// live case today is a Channel — the rewrite turns every non-trigger
	// Channel into kind=fake, and a meta tool the live kind contributed
	// (a mention lookup, a thread-history read) has no fake counterpart. Any
	// future rewrite that removes a tool source surfaces here the same way,
	// with no rule added, because the check compares the RECORDED catalog with
	// the fixture's own assembly rather than enumerating what a rewrite is
	// known to break.
	//
	// # What closes it, and what does not
	//
	// Where the removed source was a DIRECTORY the tool reads — which users a
	// channel knows — the fixture's stand-in is seeded with what the recorded
	// channel held and the offer comes back whole. That is tier 2 of the rule
	// in bronzethread's standin.go, and it is honest because every part of an
	// OFFER is our own code: the capability that contributes the tool, its
	// schema, the enum of lookup kinds, its refusals. Only which values exist
	// is stood in for, and the recorded run's own answers are where they come
	// from.
	//
	// What that does NOT license is a stand-in claiming to reproduce a REPLY it
	// renders in its own dialect; CodeStoodInToolCalled refuses exactly that.
	// Nor does it license advertising a surface nothing serves — a stand-in
	// whose declaration and whose implementation disagree is the defect the
	// fake kind's delivery surfaces are already tested against in both states.
	//
	// A source no stand-in can host still lands here, and such a session is
	// captured by hand-editing the fixture to a channel kind that really does
	// offer it, or not at all.
	CodeOfferedToolNotProducible = "offered-tool-not-producible"

	// CodeCredentialNotMintable: the emitted fixture declares a credential
	// whose type MINTS its value rather than storing one.
	//
	// Every other credential in a fixture is a placeholder Secret: the rewrite
	// substitutes fake material and the replay resolves it exactly as the live
	// run resolved real material. A MINTED type resolves nothing from that
	// Secret. Each resolve calls an external minter — a provider's token
	// endpoint, an IdP — reached through a credkind.Deps collaborator that is
	// nil wherever nobody configured one, and no fixture can configure one,
	// because the collaborator is a live network service and not a manifest.
	// The placeholder still satisfies the AgentIdentity's own Secret-exists
	// check, so the replayed identity goes Valid and every tool call that draws
	// on the credential fails at dispatch instead.
	//
	// Derived from credkind.Kind.Minted, so it is a fact about the TYPE and not
	// about any one credential: a fifth type that mints is covered the day it
	// is registered, with nothing here edited.
	//
	// HARD. Rewriting the credential to a stored type would let the bundle run,
	// and would replay a different gate than the one recorded — Minted() is
	// read by the broker's expiry rule and by identity-scoped token grants, so
	// a session that exercised a short-lived minted credential would be
	// evidence about a stored one. Such a session needs a minter the harness
	// can serve; until there is one, refusing is the honest answer.
	CodeCredentialNotMintable = "credential-not-mintable"

	// CodeProviderStateNotComputed: the transcript called a tool reaching the
	// input Channel kind's own provider, and nobody supplied that kind's
	// trigger-status reporter, so the provider values the run observed were
	// never extracted.
	//
	// The GATE on the check below, and it exists for the reason
	// CodeFixtureToolsNotComputed and CodeSecretCheckSkipped do: an empty seed
	// has two readings — "the run observed nothing from the provider" and
	// "nobody looked" — and only the second is a failure of the capture.
	// Without this gate the check below would fire on every such session and
	// name a cause that has nothing to do with the records.
	//
	// HARD, because the alternative is silence: a capture that never extracted
	// the provider's values cannot claim a stand-in seeded with none of them
	// replays the same run.
	CodeProviderStateNotComputed = "provider-state-not-computed"

	// CodeProviderStateUnseeded: the transcript called a tool reaching the input
	// Channel kind's own provider, and no revision of the triggering resource
	// could be read back out of the records to seed a stand-in with.
	//
	// # What a provider-surface tool needs, and why this is NOT a permanent limit
	//
	// Such a tool is never canned — the replay runs it, the channel kind
	// composes its reply, and (for github) a real HTTP round trip creates and
	// patches a check run. What it cannot do is INVENT the two values that came
	// from outside our code: the revision the surface resolved off the pull
	// request, and the ids the provider minted. Both are carried instead —
	// bt.Trigger.HeadSHA and bt.StandIn.TriggerStatusIDs — and both are DERIVED,
	// by asking the channel kind to read back text it composed itself
	// (channelkinds.TriggerStatusReporter.TriggerProviderStateIn).
	//
	// That is tier 2 of the rule in bronzethread's standin.go, and it is worth
	// being explicit that it does not weaken the test. The meta tool, the
	// capability that offered it, the kind, the HTTP round trip, the create and
	// the patch-BY-ID all still execute; only the number the provider chose is
	// pinned. The fixture provider's own comment makes the same argument from
	// the other side — it mints real ids precisely because the kind patches by
	// id, and a stand-in handing back 0 would let a broken round trip pass.
	//
	// So this fires only when the derivation came up EMPTY. The revision is the
	// half that decides it: the surface reads the triggering resource before it
	// does anything else, so a stand-in with nothing to report refuses every
	// call rather than answering differently. The likeliest cause is drift —
	// the kind reworded the text its own inverse reads — and the message says
	// so, because that failure is otherwise invisible.
	//
	// The tools are identified by the caller, by assembling the fixture's meta
	// tools twice and differencing on the input binding's KIND — never by
	// naming a tool or a capability here. See SelfCheckInput.FixtureTools.
	CodeProviderStateUnseeded = "provider-state-unseeded"

	// CodeUnreproducibleID: a META tool's recorded result names an identifier in
	// this repo's own minting SHAPE — a family prefix and the 16 hex digits every
	// crypto/rand mint site emits — that no injectable seam reproduces.
	//
	// # Why the shape is the rule, and not a list of tools
	//
	// The replay runs a meta tool for real, so its reply is composed by our own
	// code from our own inputs — with one exception: an id minted DURING the run.
	// Every family with a seam is pinned (bt.MintedIDFamilies, recorded in
	// Bundle.MintedIDs). Anything else of that exact shape was minted by a
	// component with no seam, so the replay mints a different one, and the step's
	// expectation was derived from the recorded bytes. That is a bundle which
	// emits clean and then fails inside the suite, naming the driver.
	//
	// Matching on the SHAPE rather than on tool names is what makes this
	// general: every mint site in this repo emits 8 random bytes hex-encoded
	// behind a prefix, so a component that grows a new id is caught the day it
	// appears, with nothing here edited. A pinned id is exempted by being IN
	// Bundle.MintedIDs, not by being named — so a family that gains a seam stops
	// firing for free, and one whose seam is removed starts firing again.
	//
	// # Two rules, because one shape does not fit every family
	//
	// The generic rule above is a prefix and 16 hex digits. A render handle is
	// not that — the session name sits between its prefix and a six-hex tail —
	// so the check ALSO walks the decoded result with the fold's own matcher
	// (familyShapedIDsIn), which asks each registered family. That is what makes
	// "recognizable" and "collectable" the same set: an id the capture could
	// have pinned and did not is refused, and an id it could not have pinned is
	// not silently invented.
	//
	// The case that drove this — artifact_prepare's `revision_id` and `handle`,
	// both in one result — is now pinned rather than refused. What refuses today
	// is a result carrying an id NO family claims, or one a family claims that
	// the bundle failed to record.
	//
	// HARD. There is no honest way to emit such a bundle: narrowing the
	// expectation around the id would drop the claim about the one call the
	// scenario was built on, and canning the reply would serve a regression in
	// the meta tool its own recorded answer.
	CodeUnreproducibleID = "unreproducible-id"

	// CodeStoodInToolCalled: the transcript CALLED a tool the emitted fixture
	// can only OFFER through a seeded stand-in.
	//
	// The complement of CodeOfferedToolNotProducible, and the boundary of the
	// fix that closed it. Seeding the fake kind's user directory restores the
	// OFFER a rewrite destroyed, faithfully: every part of the offer is our own
	// code and runs. It does not restore the ANSWER — a reply is the kind's own
	// rendering of the resolved value, and the stand-in kind renders it
	// differently from the kind that recorded it, so an expectation derived
	// from the recorded reply describes bytes the replay cannot produce.
	//
	// HARD, and deliberately not closed by teaching the stand-in to render like
	// the kind it replaced: that is simulating a counterparty, which is the line
	// the fake kind's own delivery-surface doc draws and refuses to cross.
	CodeStoodInToolCalled = "stood-in-tool-called"

	// CodeUnseededAllow: an allowed authz decision whose expansion yielded
	// nothing to seed AND nothing to subtract, so the replay fixture will not
	// reproduce the grant.
	//
	// Two exemptions, both about a grant the REPLAY writes for itself. An
	// expansion whose tuples were all subtracted is one (DeriveSeed's three
	// subtraction classes). A pair the class declares as an authz SLOT is the
	// other, and it exists because a session-only slot binding is COLLECTED
	// when the session ends: by capture time the tuple is gone, so the
	// expansion is empty for the same reason it would have been subtracted had
	// it still been there. See collectedSlotGrant.
	//
	// HARD, and deliberately still hard for everything else. A permission
	// matching no declared slot really is a grant the fixture cannot reproduce,
	// and the divergence would otherwise surface minutes later at replay, in
	// the wrong layer. The message says which of the two cases the reader is
	// looking at — see unseededSlotHint.
	CodeUnseededAllow = "unseeded-allow"

	// CodeTruncatedTrigger: the recorded delivery body was cut at
	// triggerdelivery.MaxBodyBytes. A truncated body cannot be re-signed into a
	// delivery that passes HMAC verification.
	CodeTruncatedTrigger = "truncated-trigger"

	// CodeUnroutableToolCall: a tool call that resolved to NO declared transport
	// — not an MCP server, not a sidecar toolbox, not one of the class's
	// toolBundles — and is not a meta tool.
	//
	// It used to be called sandbox-tool-call and it used to mean the sandbox
	// case specifically, because the sandbox WAS unroutable: the driver fed
	// toolOutputs to its one fake MCP server and nowhere else. That is no longer
	// true — a sandbox result now reaches the fake exec binder — so what remains
	// is the residue: a name carrying a prefix nothing in the fixture declares.
	// In practice that means the capture failed to gather something (a class it
	// could not read) rather than the format lacking a route.
	CodeUnroutableToolCall = "unroutable-tool-call"

	// CodeSandboxToolUnresolved: the transcript called a tool of a DECLARED
	// toolBundle, but the capture gathered no SpiceboxClass / SpiceboxToolspec
	// pair yielding that class tool.
	//
	// The prefix resolving is not enough. A sandbox tool exists at replay only
	// if the emitted fixture declares the class tool it is synthesized from, and
	// the fixture declares only what was gathered. Left alone, the bundle would
	// carry a recorded output for a tool the replayed class never offers, and
	// the model's scripted call would come back "unknown tool" — a failure that
	// names the tool but says nothing about the manifest that was never written.
	CodeSandboxToolUnresolved = "sandbox-tool-unresolved"

	// CodeUnreplayableSandboxResult: a recorded sandbox result matched no
	// composition the fold can invert, so no bt.SandboxOutput can be derived.
	//
	// Two unrelated causes, reported as two different findings because the
	// remedy differs and a message describing one misdescribes the other.
	//
	// A FAILED call whose terminal condition an exit code cannot reach. The
	// ordinary non-zero exit IS captured now — bt.SplitSandboxFailure inverts
	// "ToolCall failed: NonZeroExit — exit code N" plus its stderr tail into
	// SandboxOutput{ExitCode, Stderr}, and the replayed call re-composes the
	// identical bytes from the code under test. What remains are the conditions
	// no exit code produces: a Timeout or a Cancellation our own watchdog
	// wrote, an exec error, and a truncated stderr tail whose bytes are gone.
	//
	// A SUCCESSFUL call composed by a STREAMING or INTERACTIVE toolkit whose
	// composed result cannot honestly be served back. The ordinary streaming
	// result IS captured now — bt.SplitStreamResult carries it verbatim and the
	// replay serves it as the toolkit's own output (see
	// CodeStreamResultServedVerbatim for what that costs). What remains are the
	// two it refuses: an IDLE exit, whose Terminal=true ends the turn, so
	// serving it as an ordinary result would let the replay run on past where
	// the recording stopped; and a result over bt.StreamResultBudgetBytes,
	// which the replayed tool's bounded stdout tail would truncate at the head.
	CodeUnreplayableSandboxResult = "unreplayable-sandbox-result"

	// CodeStreamResultServedVerbatim: a captured STREAMING or INTERACTIVE
	// toolkit's result is carried in the bundle and handed back, rather than
	// re-composed by the code that produced it.
	//
	// A warning, because the bundle is legitimate: the toolkit is a black box,
	// the recorded call goes in and the recorded result comes back, which is
	// the same contract every canned MCP result takes. But it is not the SAME
	// as canning an MCP server's response, and the difference is worth a
	// finding rather than a reader's inference: both sides of an MCP boundary
	// are external, while HALF of this one is ours. The stream parse, the
	// terminal-event decode, and the "(<duration>, $<cost>)" header derived
	// from it are not exercised by a bundle that serves the text they produced.
	//
	// The rest of the path still is — the ToolCall, the streaming gateway, the
	// bridge, and composeStreamResult's no-terminal-result fallback, which
	// wraps the served text in a fresh "(exit N)" header. So a replayed result
	// visibly carries BOTH headers, which is what lets a reader tell a bundle
	// that replays a recording from one that reproduced it.
	CodeStreamResultServedVerbatim = "stream-result-served-verbatim"

	// CodeMetaReplyCanned: a META tool's reply is carried in the bundle and
	// handed back, rather than composed by the tool that produced it.
	//
	// A warning, and the closest sibling in this file is
	// CodeStreamResultServedVerbatim — but the two are not equally comfortable,
	// and the difference is worth stating rather than leaving to a reader. A
	// streaming toolkit is a black box on the far side of a process boundary;
	// a meta tool is OURS, start to finish. So this is the only finding here
	// that reports coverage of this repo's own code being given up, which is
	// why bt.MetaToolCannable narrows it to the four tools whose own table
	// declares that their answer comes from state a replay has no way to hold.
	//
	// What still runs is most of the path and is worth naming: the capability
	// that offered the tool, its presence in the recorded catalog, the system
	// prompt's listing of it, introspect_tool's index over it, the authz check
	// and the plan gate against its declared permission, the approval flow, and
	// every hook in the pipeline. A refused call never reaches the canned bytes
	// at all, because the gate returns before Execute. What stops running is
	// the tool's own body: its argument parsing and refusals, the query or
	// search it issues, the per-datum audience filtering on what comes back,
	// and the rendering of the result the model is handed.
	//
	// It is a warning rather than a refusal because the bundle is still
	// legitimate — the recorded call goes in and the recorded answer comes
	// back, which is the contract every canned MCP result already takes — and
	// because the alternative is refusing the capture of any session that ever
	// searched its own memory. The rule that keeps it honest is not this
	// finding but Bundle.Validate: a canned reply with no Assert.ToolsCalled
	// entry is refused outright, so a bundle that cans a tool always also
	// states that the tool was called.
	CodeMetaReplyCanned = "meta-reply-canned"

	// CodeMetaReplyNotUniform: one cannable META tool whose calls did not all
	// answer the same way — two successes with different bodies, or a success
	// beside an error.
	//
	// The exact shape CodeToolErrorNotUniform is, and hard for the same reason:
	// bt.MetaToolReply is ONE body per tool name, served on every call, so a
	// tool that varied has no spelling in the format. Canning the first body
	// would serve it to a call that recorded something else, and the step
	// derived from that other call would fail somewhere downstream naming the
	// tool rather than the capture.
	//
	// Reachable the moment a session searches its memory twice with different
	// queries, which is the ordinary way these tools are used. Closing it means
	// a per-call sequence for meta replies — the counterpart of
	// ToolOutputSequence — which is a feature with its own design, and one that
	// should not be built before a session needs it.
	CodeMetaReplyNotUniform = "meta-reply-not-uniform"

	// CodeStreamResultCollision: two sandbox tools of the SAME toolBundle both
	// recorded a streaming result, and one bundle is one sandbox pod.
	//
	// Hard, because the replay has no way to tell the two apart. The fake exec
	// binder's stream half is keyed by POD and hands its driver no argv — the
	// request-inspecting responder that routes an ordinary sandbox call by
	// command line has no streaming counterpart — so a pod serving two recorded
	// streams would answer both tools with whichever one was registered. The
	// bundle would replay green while one tool was served the other's output.
	CodeStreamResultCollision = "stream-result-collision"

	// CodeSecretOutputRecorded: a captured sandbox tool PRODUCED a secret
	// output, and what the bundle carries for it is the diversion's description
	// line rather than process output.
	//
	// A warning, not a refusal, and the distinction rests on where the secret
	// went. The runner diverts the value out-of-band BEFORE the tool_result
	// block is built, so neither the model nor session memory ever saw it and
	// there is nothing sensitive to leak into the bundle. What the transcript
	// holds is "<description>\n<secret-output name=… ref=… bytes=N>", and
	// RewriteFixture drops spec.secretOutput so the replayed call composes an
	// ordinary process result from that description.
	//
	// For a `file:`-sourced output the description IS the producer's real
	// stdout, and the round trip is faithful. For a `stdout:`-sourced one the
	// description comes from the toolspec and the process's actual stdout WAS
	// the secret — correctly diverted, and not recoverable from any record — so
	// the bundle's "stdout" is spec prose. The reader is told rather than left
	// to work it out, because nothing in the emitted bundle distinguishes the
	// two.
	CodeSecretOutputRecorded = "secret-output-recorded"

	// There is deliberately no finding for a respond_to_user carrying an
	// `attached` artifact any more. attached-lost used to be a hard one, on the
	// ground that bt.ReplyPart.Text is shorthand for a text-only reply and
	// dropped everything else — so a bundle whose point was artifact DELIVERY
	// silently stopped proving delivery.
	//
	// Nothing is dropped now: foldReply emits the shorthand only for a call
	// whose arguments are exactly a non-empty text, and any other
	// respond_to_user as an ordinary bt.ReplyPart.ToolUse carrying its full
	// arguments verbatim. The check could no longer fire, and a check that
	// cannot fire reads as coverage, so it is gone rather than kept unreachable.
	// See foldReply's "Which shape a respond_to_user takes".

	// RETIRED: CodeSecretGatedSidecar ("secret-gated-sidecar"). It refused a
	// transcript that called a tool of a sidecar declaring spec.secretInputs,
	// because such a sidecar's tools are synthesized only once the per-session
	// secret-output Secret exists, and a bundle has no producer to write one.
	//
	// RewriteFixture now DROPS spec.secretInputs from the emitted CR, so the
	// gate is not declared at replay and the sidecar's tools are synthesized
	// like any other's. That is a fixture rewrite of the same class as replacing
	// an MCPServer's real auth with a placeholder — at replay the sidecar IS the
	// fake MCP stub and needs no credential — not the driver manufacturing a
	// Secret behind a gate that still stands. See rewriteSidecarToolboxes.
	//
	// The check could no longer fire, and a check that cannot fire reads as
	// coverage, so it is gone rather than kept unreachable.

	// CodeUnsubstitutedMintedID used to report an emitted tool-call argument
	// carrying a LITERAL run-minted id where a sentinel belonged.
	//
	// The sentinel scheme is gone. Bundle.MintedIDs records the ids the run
	// minted and the replay's own minters hand the same values back, so a
	// literal minted id in an argument is now the CORRECT emission rather than a
	// defect — and the shape the check existed for (update_plan minting one
	// operation per in-progress item, nested inside its item array, which a
	// one-value-per-sentinel table could not express) is what the recorded
	// sequence carries directly.
	//
	// The check could no longer fire, and a check that cannot fire reads as
	// coverage, so it is gone rather than kept unreachable. What replaces it is
	// not a check at all but an assertion at replay: the sequence is exhausted
	// if the run mints more, left over if it mints fewer.

	// CodeMCPNameCollision: two declared MCP prefixes expose the same
	// server-side tool name. toolOutputs is keyed server-side (that is what
	// h.MCP.OnTool registers), so their outputs collapse into one sequence the
	// single fake stub cannot tell apart.
	CodeMCPNameCollision = "mcp-name-collision"

	// CodeEmptyExpect: a bundle step whose Expect is zero-valued. checkExpect
	// then asserts NOTHING, leaving that step of a positional replay entirely
	// unguarded — which defeats the divergence contract the format rests on.
	CodeEmptyExpect = "empty-expect"

	// CodeNoTriggerRecord: a session that opened on a trigger channel but has
	// no trigger_delivery record. Warn, not hard: the bundle falls back to
	// userTurns, which is still worth emitting.
	CodeNoTriggerRecord = "no-trigger-record"

	// CodeTriggerChannelUnknown is CodeNoTriggerRecord's mirror, and hard where
	// that one is a warning: the delivery WAS recorded but the capture was not
	// told which input Channel it arrived on.
	//
	// Both halves of the trigger handling then go silently missing. bt.Trigger
	// cannot be emitted, because the driver signs the payload with the named
	// Channel's own credentials and there is no name; and the assembly step's
	// suppression of the synthesized index-0 turn is keyed off the same pair,
	// so the prompt the RUNNER wrote is emitted as a userTurn — a message the
	// bundle claims a person typed. Neither is visible in the emitted files.
	CodeTriggerChannelUnknown = "trigger-channel-unknown"

	// CodeNoAgentReply: the session never spoke to the user, so the bundle can
	// assert nothing about what the agent said. Warn: a session whose evidence
	// is its authorization trace needs no reply.
	CodeNoAgentReply = "no-agent-reply"

	// CodeUndeterminedApprovalCategory: a decided approval whose interaction
	// category is not readable from any durable record (see
	// DeriveApprovalSettings' fourth return value). Warn: a human adds it to
	// autoApprove or autoDeny by hand, having looked at what it actually was —
	// without which the replay hangs waiting for an approver.
	CodeUndeterminedApprovalCategory = "undetermined-approval-category"

	// CodeBundleInvalid: the assembled bundle fails bt.Bundle.Validate — the
	// SAME method the replay driver refuses on. Hard, and it must be: without
	// it a capture reports no findings, writes bundle.json, and the loader
	// rejects the file minutes later in a different suite, pointing at the
	// bundle rather than at the capture that assembled it.
	//
	// The shape that motivated it: a session opened by a webhook that a person
	// ALSO replied in. Only the SYNTHESIZED index-0 turn is suppressed, so
	// UserTurns stays non-empty while Trigger is set, and a bundle may start
	// one way or the other but never both.
	//
	// Checked against the assembled bundle rather than re-derived from Folded,
	// for the reason PlaceholderTools gives: the finding must describe the
	// bytes a reader is holding, not a second derivation free to disagree.
	CodeBundleInvalid = "bundle-invalid"
)

// Finding is one reason a capture is not what it appears to be.
type Finding struct {
	Severity Severity
	Code     string
	// Message names the SPECIFIC offending thing — the tool, the turn index,
	// the file, the key. A finding that says "something is wrong" costs more
	// than it saves, because the reader goes back to the logs anyway.
	Message string
}

// LiveSecret is one live credential value the capture read, paired with a name
// the operator will recognize. The value is compared against the emitted bytes
// and is NEVER echoed into a Finding.Message: the findings are printed to a
// terminal and land in whatever captured that terminal, so a check that exists
// to catch a leaked credential must not leak one itself.
type LiveSecret struct {
	// Name identifies the secret to a human, e.g. "demo-agent-creds/bot-token".
	// For an EmptyRead entry it is the bare Secret name, because there is no
	// key to qualify it with.
	Name string
	// Value is the live material. An EMPTY value is skipped rather than
	// matched: "" is a substring of every file, so matching it would turn a
	// Secret key holding no bytes into a hard finding on every emitted file.
	//
	// Because it is skipped, an empty Value is NOT SCANNABLE and does not count
	// toward the check having run: an entry carrying one is indistinguishable
	// from not looking at all, so a fixture bearing a Secret whose every entry
	// is empty raises CodeSecretCheckSkipped exactly as a nil slice does. A
	// caller populating this struct should treat an empty Value as a failure to
	// read the Secret, not as a value that happens to be blank — unless it sets
	// EmptyRead below, which says exactly that it is not a failure.
	Value string

	// EmptyRead records that the capture READ this Secret and it held no
	// credential material at all: no data keys, or every key blank.
	//
	// It exists because "nobody looked" and "somebody looked and there was
	// nothing there" are different answers to the gate's question, and only the
	// first is a failure. A live cluster produces the second routinely — the
	// per-session Channel credentials Secret a browser-started session gets is
	// created EMPTY, because the browser surface needs no credential, and the
	// fixture still emits a placeholder standing in for it. Refusing every
	// browser-started capture over a Secret that is empty by design is the same
	// false positive the coarse gate produced, one layer down.
	//
	// An entry carrying it is still never SCANNED: its Value is empty, and ""
	// is a substring of every file. It counts only toward the gate, and only a
	// caller that really performed the read may set it.
	EmptyRead bool

	// Public records that the type owning this Secret's SHAPE declares this
	// data key a PUBLIC identifier rather than credential material. Such an
	// entry counts toward the gate — the read happened — but its value is not
	// matched against the emitted bytes.
	//
	// It exists because "this value came out of a Secret" is provenance, not
	// sensitivity, and a credential bundle routinely mixes the two. A GitHub
	// App's Secret holds a private key and a webhook secret next to an app id
	// and an installation id; GitHub publishes the latter two, and it puts the
	// installation id in the BODY of every webhook delivery it sends. Without
	// this distinction the capture refuses every triggered GitHub session
	// permanently, for a value the payload cannot be written down without — and
	// no amount of redaction fixes that, because payloads/delivery.json exists
	// precisely to be the verbatim record of what was delivered.
	//
	// FAIL CLOSED: the zero value is false, so a caller that does not know, or
	// forgets to ask, gets the whole Secret scanned. Only an explicit
	// declaration by the kind that defines the Secret's shape sets it — see
	// credkind.Kind.PublicSecretKeys and channelkinds.Kind.PublicSecretKeys.
	//
	// This narrows only the KNOWN-VALUE scan. checkStructuralSecrets is
	// untouched by it and reads the same bytes with no gate at all, so a key
	// wrongly declared public whose value carries a recognizable credential
	// shape — a PEM header, a vendor-prefixed token — still refuses the
	// capture. A declaration cannot be used to wave material past the scan;
	// it can only stop an exact-value match on bytes that are public anyway.
	Public bool
}

// FixtureTool is one meta tool the REPLAY will offer, and the single fact the
// checks need about it beyond its name.
//
// A struct rather than two parallel string slices because the two facts are
// answered by one act — assembling the fixture's capabilities — and two slices
// could disagree about which names that act produced.
type FixtureTool struct {
	// Name is the LLM-facing tool name.
	Name string

	// ExternalSurface reports that this tool exists because the session's INPUT
	// Channel kind advertises a surface on its own PROVIDER — a pull request's
	// check run, not anything inside the cluster — so its result at replay
	// comes from a stand-in provider that mints its own state.
	//
	// The caller derives it by DIFFERENCE, not by naming anything: assemble the
	// fixture's meta tools once with the input binding as it is, once with that
	// binding's kind blanked, and whatever disappears is what the kind itself
	// contributed. Consulted only when the channel-kind registry says that kind
	// reports trigger status, which is the registered fact that makes "the kind
	// contributed it" mean "it reaches the provider".
	//
	// FAIL CLOSED: the zero value is false, so a caller that does not know, or
	// forgets to ask, gets a tool treated as ordinary. That direction is right
	// here because the SEPARATE gate on this list is FixtureTools being empty:
	// a caller that computed nothing is caught by CodeFixtureToolsNotComputed
	// before any per-tool fact is read.
	ExternalSurface bool

	// StoodIn reports that this tool exists in the fixture's assembly ONLY
	// because a stand-in was seeded to put it back — today, the fake kind's
	// seeded user directory replacing the one the rewrite to kind=fake took
	// away.
	//
	// The distinction it draws is between an OFFER a stand-in can reproduce and
	// an ANSWER it cannot. Seeding which users exist restores the offer exactly:
	// the capability assembly, the tool's schema, its enum of lookup kinds and
	// its three refusals are all our code, and all of it runs. The REPLY is a
	// different matter — it is the kind's own RenderMention of the resolved id,
	// and a stand-in kind renders it differently from the kind that recorded it.
	// So a bundle may be OFFERED a stood-in tool and must not have CALLED one;
	// checkStoodInCalls is that rule.
	//
	// Derived by the same difference the field above is: assemble once with the
	// seed and once without it, and whatever appears was put back by the seed.
	// No tool is named and no capability is.
	StoodIn bool
}

// SelfCheckInput is everything the checks read. A plain struct of already-
// derived values rather than a live reader, for the same reason Records is:
// every rule below is testable against synthetic input with no cluster.
type SelfCheckInput struct {
	// Records and Folded are the capture's two halves: what the session
	// durably recorded, and what the fold made of it.
	Records Records
	Folded  Folded

	// Seed is DeriveSeed's result, read for its Unseeded keys.
	Seed SeedResult

	// Files are the fixture files RewriteFixture emitted, post-redaction.
	//
	// Read for ONE question now — does the fixture emit a Secret, which is what
	// tells an empty LiveSecrets that somebody failed to look. The scanning
	// itself moved to Emitted; see checkSecrets. Kept as parsed YAML documents
	// rather than folded into Emitted because that question is answered by
	// unmarshalling a document's kind, not by looking at bytes.
	Files []FixtureFile

	// Emitted is EVERY file the capture will write, final bytes, exactly as
	// WriteResult will write them. Both secret scans run over this and nothing
	// else.
	//
	// It exists because scanning anything narrower has already failed here. The
	// leak scan used to read Files alone — the fixture manifests — which is one
	// of the four surfaces a capture writes. bundle.json went unscanned, and
	// bundle.json is where a credential an upstream TOOL returned lives; so did
	// the trigger payload, which is a whole webhook body. A capture could write
	// a live credential into a repo and report clean, and did so by
	// construction rather than by any oversight in the check itself.
	//
	// Over the FINAL bytes, not the objects: a value that only appears once the
	// bundle is marshalled — escaped inside a tool result, in a field no
	// rewrite rule mentions — is in no object a check could inspect.
	Emitted []EmittedFile

	// DeclaredTools maps each declared MCPServer's prefix (its metadata.name,
	// the same value FoldOptions.MCPPrefixes carries) to the SERVER-side tool
	// names its spec.tools allowlist declares.
	DeclaredTools map[string][]string

	// SandboxTools maps each declared toolBundle's name (the LLM-facing prefix,
	// the same value FoldOptions.SandboxPrefixes carries) to the SpiceboxClass
	// tool names its toolspecs resolve to.
	//
	// The sandbox counterpart of DeclaredTools, and separate for the reason the
	// fold keeps the two prefix lists separate: the transports differ in what a
	// replay has to do with a recorded result. A prefix present here with an
	// EMPTY list is not the same as absent — it means the class declares the
	// bundle but the capture gathered no SpiceboxClass or SpiceboxToolspec that
	// yields a tool, so the fixture will declare no such tool at replay. That is
	// its own finding; see checkSandboxTools.
	SandboxTools map[string][]string

	// MetaTools are the LLM-facing names that RUN FOR REAL at replay and
	// therefore need no canned output — the meta tools the session's
	// capabilities actually offered.
	//
	// Supplied by the caller rather than enumerated here: which meta tools a
	// session gets is decided by its capabilities (see
	// pkg/agent/tool/meta/capability), so a list transcribed into this package
	// would be a snapshot that drifts silently the first time a capability
	// gains a tool. An EMPTY list is fail-closed on purpose — every non-MCP
	// call then reads as a sandbox call and the capture refuses, which is the
	// safe direction to be wrong in.
	//
	// respond_to_user is known unconditionally and does not belong here: the
	// fold gives it dedicated handling, so a caller has no reason to list it.
	MetaTools []string

	// FixtureTools are the meta tools the REWRITTEN fixture will offer — the
	// same capability assembly MetaTools comes from, run a second time against
	// the Channel, Secret and kind RewriteFixture emits rather than the live
	// ones.
	//
	// The pair is the point. MetaTools says what the run WAS offered and
	// FixtureTools says what the replay WILL be offered, and the gap between
	// them is a tool the fixture rewrite destroyed. Neither list alone can show
	// it: a check reading only the live assembly compares the session to
	// itself, the same defect CapturedSkills exists to avoid.
	//
	// Supplied by the caller for the reason MetaTools is — the assembly is
	// driven by the runner's own capability registry, and a list transcribed
	// into this package would be a snapshot that drifts. Empty raises
	// CodeFixtureToolsNotComputed rather than passing: see that code for why an
	// unsupplied list must not read as "the fixture offers nothing".
	FixtureTools []FixtureTool

	// LiveSecrets are the credential values the capture read while gathering
	// the live manifests and the session's resolved status.
	//
	// REQUIRED for every name in ShadowedSecrets. RewriteFixture deliberately
	// never sees a live Secret's value, so the caller that gathered the
	// manifests must also read the referenced Secrets — for the sole purpose
	// of proving they are absent — and pass them here. Leaving one out does
	// NOT pass the scan: it raises CodeSecretCheckSkipped, because a scan with
	// nothing to look for proves nothing, and reporting that as clean is how a
	// live credential gets committed to a repo.
	//
	// A caller is expected to supply MORE than ShadowedSecrets requires. A
	// credential can leak into the transcript without the fixture ever emitting
	// a Secret that stands in for it — the resolved model key, the per-session
	// passthrough credentials a tool bundle draws on, a sidecar toolbox's
	// materialized upstream token — and bundle.json is exactly where such a
	// value lands. ShadowedSecrets is the floor, not the target.
	LiveSecrets []LiveSecret

	// ShadowedSecrets are the emitted placeholder Secrets whose name came from
	// a live reference, straight from RewriteResult.ShadowedSecrets.
	//
	// This is the gate on LiveSecrets, and it is the independent fact that
	// separates "there was nothing to read" from "nobody read it" — the same
	// shape GuardConfigured has for TransformedToolUseIDs. A name here is a
	// claim by the rewrite that it substituted a placeholder for material that
	// really exists, so the absence of a matching LiveSecret is a scan that did
	// not run rather than a scan that found nothing.
	//
	// Read from the rewrite rather than re-derived here, so the list of Secrets
	// standing in for live material and the list checked for cannot disagree.
	ShadowedSecrets []string

	// GuardConfigured reports whether the captured AgentClass configured a tool
	// guard (AgentClass.spec.toolGuard non-nil), read from the same live
	// manifests the fixture rewrite already consumes.
	//
	// It exists for one purpose: to tell an empty Records.TransformedToolUseIDs
	// that means "no guard rewrote anything" from one that means "nobody
	// gathered the guard audits". With a guard configured, the second reading
	// is a hard finding — see CodeGuardScanSkipped.
	GuardConfigured bool

	// ClassSkills are the canonical skill names the captured AgentClass opts
	// into (AgentClass.spec.skills[].ref), read from the same live manifests
	// the fixture rewrite consumes rather than passed in beside them.
	//
	// It carries the NAMES rather than a count so the finding can say which
	// skills a reader would have to hand-add, which is the only thing that
	// makes the refusal actionable rather than a dead end.
	ClassSkills []string

	// CapturedSkills are the canonical names the fixture rewrite actually
	// emitted a Skill CR for (RewriteResult.EmittedSkills), and the check is
	// the set difference against ClassSkills.
	//
	// Reported by the rewrite rather than re-derived here, for the reason
	// ShadowedSecrets and UngatedTools are. This check's whole job is to
	// notice a gap between what the class opted into and what the fixture
	// carries; a value re-derived from the class could never disagree with the
	// class, so it could never see the gap.
	CapturedSkills []string

	// SkillsWithoutBundle are the canonical names whose spec.bundle ref the
	// rewrite dropped (RewriteResult.SkillsWithoutBundle). See
	// CodeSkillBundleNotStaged.
	SkillsWithoutBundle []string

	// SkillElisions are the skill-source substitutions the capture APPLIED, as
	// recorded on the bundle. Nil for the ordinary capture, which elides
	// nothing. See CodeSkillContentElided.
	SkillElisions []bt.SkillElision

	// MintedCredentials are the emitted credentials whose type mints its value
	// instead of storing one (RewriteResult.MintedCredentials).
	//
	// From the rewrite that emitted them, never re-derived from the live
	// AgentIdentity: the check's whole subject is what the FIXTURE will carry,
	// and a rewrite that learned to drop or convert such a credential would
	// stop reporting it in the same commit. See CodeCredentialNotMintable.
	MintedCredentials []MintedCredential

	// TriggerProviderMissing reports that the capture had no trigger-status
	// reporter for the input Channel's kind, so no provider value could be read
	// back out of the records.
	//
	// Stated by the CALLER rather than inferred from an empty seed, and the
	// distinction is the whole point: an empty seed on a session whose kind
	// reports no trigger status at all is correct and silent, while an empty
	// seed because nobody resolved the reporter is a hole. Only the caller
	// knows which. See CodeProviderStateNotComputed, and the same shape in
	// CodeFixtureToolsNotComputed and CodeSecretCheckSkipped.
	TriggerProviderMissing bool

	// TriggerChannel is FixtureInput.TriggerChannel — the input Channel a
	// triggered session opened on, empty for a session a person typed into.
	// It is how the check knows a MISSING trigger record is missing rather
	// than simply absent.
	TriggerChannel string

	// UndeterminedApprovals is DeriveApprovalSettings' fourth return value:
	// decided approvals whose interaction category no durable record names.
	UndeterminedApprovals []string

	// Bundle is the ASSEMBLED bundle, exactly as it will be written.
	//
	// Passed in whole so checkBundle can run bt.Bundle.Validate — the replay
	// driver's own precondition — over the same value that lands on disk. A
	// zero Bundle therefore fails validation and raises CodeBundleInvalid,
	// which is the fail-closed direction: a caller that forgot to supply it
	// gets a loud finding, never a silent pass.
	Bundle bt.Bundle

	// PlaceholderTools are the declared tools addPlaceholderToolOutputs gave a
	// synthesized toolOutputs entry to, sorted.
	//
	// Supplied by the assembly step rather than recomputed here. The check
	// reports what was actually EMITTED, and only the caller that wrote those
	// entries can say what that was — recomputing the set from Folded and
	// DeclaredTools would be a second derivation free to disagree with the
	// bundle a reader is holding.
	PlaceholderTools []string
}

// SelfCheck reports every way this capture would replay differently than it
// recorded.
//
// A pure function of its input, and deterministic: two runs over one capture
// produce the same findings in the same order. Several checks read maps, and a
// set ranged over directly would reorder between runs — a capture is a
// reproducibility artifact, and a report whose own diff is noise gets skimmed
// instead of read.
//
// Order is stable but carries no meaning; callers group by Severity themselves.
// HasHardFinding is the single question the command exits on.
func SelfCheck(in SelfCheckInput) []Finding {
	var out []Finding
	out = append(out, checkDeclaredTools(in)...)
	out = append(out, checkUnmappedTurns(in)...)
	out = append(out, checkTransformedOutputs(in)...)
	out = append(out, checkToolErrors(in)...)
	out = append(out, checkToolCalls(in)...)
	out = append(out, checkFixtureTools(in)...)
	out = append(out, checkExternalSurfaceCalls(in)...)
	out = append(out, checkStoodInCalls(in)...)
	out = append(out, checkUnreproducibleIDs(in)...)
	out = append(out, checkFixtureCredentials(in)...)
	out = append(out, checkSandboxResults(in)...)
	out = append(out, checkMetaReplies(in)...)
	out = append(out, checkSteps(in)...)
	out = append(out, checkSecrets(in)...)
	out = append(out, checkStructuralSecrets(in)...)
	out = append(out, checkSkills(in)...)
	out = append(out, checkSeed(in)...)
	out = append(out, checkTrigger(in)...)
	out = append(out, checkReplies(in)...)
	out = append(out, checkApprovals(in)...)
	out = append(out, checkBundle(in)...)
	return out
}

// HasHardFinding reports whether anything in f stops the capture.
//
// Warnings are ignored deliberately: a triggered session captured before
// trigger_delivery shipped, or a session that never spoke to a person, is still
// worth emitting — what such a bundle claims is true, it just claims less.
func HasHardFinding(f []Finding) bool {
	return slices.ContainsFunc(f, func(x Finding) bool { return x.Severity == SeverityHard })
}

// checkDeclaredTools raises both findings that read the MCPServer allowlists:
// the synthesized-placeholder notice, and two servers declaring one server-side
// name.
//
// There is deliberately no hard finding for "a declared tool with no recorded
// output" any more. There used to be, and it could not be made accurate: a
// placeholder serves that case in every shape it takes, so the check would have
// been unreachable — and a check that cannot fire is worse than none, because
// it reads as coverage. See CodePlaceholderToolOutputs.
func checkDeclaredTools(in SelfCheckInput) []Finding {
	var out []Finding

	// Prefixes sorted so both the findings below and splitMCPName's own
	// resolution (one prefix can be a prefix of another) are stable.
	prefixes := slices.Sorted(maps.Keys(in.DeclaredTools))

	declaredBy := map[string][]string{} // server-side tool name -> prefixes declaring it
	for _, prefix := range prefixes {
		for _, tool := range in.DeclaredTools[prefix] {
			declaredBy[tool] = append(declaredBy[tool], prefix)
		}
	}

	if len(in.PlaceholderTools) > 0 {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodePlaceholderToolOutputs,
			Message: fmt.Sprintf("toolOutputs entries for %v were SYNTHESIZED, not observed: the fixture's "+
				"MCPServers declare them and the transcript recorded no result for them, so each got an "+
				"empty object. They exist only to satisfy allowlist validation at class admission — without "+
				"an entry the class fails as AgentClassMCPServerInvalid/AllowlistDrift, and a tool the stub "+
				"has no handler for is missing from its tools/list — and the replay never SERVES one: it "+
				"either never calls the tool, or the gate refuses the call before dispatch, or a toolErrors "+
				"registration wins over it. This is what a human writes by hand into an authored bundle; it "+
				"is reported so a reader can tell an invented entry from a recorded one.", in.PlaceholderTools),
		})
	}

	for _, tool := range slices.Sorted(maps.Keys(declaredBy)) {
		servers := declaredBy[tool]
		if len(servers) < 2 {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeMCPNameCollision,
			Message: fmt.Sprintf("MCPServers %v each declare a tool named %q; toolOutputs is keyed by the "+
				"SERVER-side name, so their recorded outputs collapse into one sequence the single fake "+
				"MCP server cannot tell apart. The bundle would only replay if the whole interleaving "+
				"reproduced exactly.", servers, tool),
		})
	}
	return out
}

// checkUnmappedTurns reports each hole individually. Collapsing them into one
// line would make a reader fix the first and re-run to discover the second.
func checkUnmappedTurns(in SelfCheckInput) []Finding {
	var out []Finding
	for _, idx := range in.Folded.UnmappedTurns {
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeUnmappedTurn,
			Message: fmt.Sprintf("transcript turn %d mapped to no bundle step; the replay would simply not "+
				"contain it, and nothing at replay time would say so", idx),
		})
	}
	return out
}

// checkTransformedOutputs reports each guard-rewritten output, and refuses
// first when it cannot tell that it looked.
//
// Records.TransformedToolUseIDs is externally gathered — see its doc comment —
// so an empty map has two readings: no guard rewrote anything, or nobody
// gathered the audits. GuardConfigured separates them, exactly as an emitted
// Secret separates the two readings of an empty LiveSecrets.
func checkTransformedOutputs(in SelfCheckInput) []Finding {
	var out []Finding

	if in.GuardConfigured && len(in.Records.TransformedToolUseIDs) == 0 {
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeGuardScanSkipped,
			Message: "the transformed-output scan did NOT run: the captured AgentClass configures a tool guard, " +
				"so a guard was in a position to rewrite tool output, but Records.TransformedToolUseIDs is " +
				"empty. That is indistinguishable from a gather that found the audit kind absent, queried the " +
				"wrong scope, or swallowed an error — and a clean result here would prove nothing. Populate " +
				"Records.TransformedToolUseIDs from the contentguard / toolguard / infoleakage audits.",
		})
	}

	for _, id := range slices.Sorted(maps.Keys(in.Records.TransformedToolUseIDs)) {
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeTransformedOutput,
			Message: fmt.Sprintf("tool call %q had its output rewritten by %s; the recorded value is "+
				"POST-transform and a replay would transform it again. The capture cannot invert that.",
				id, in.Records.TransformedToolUseIDs[id]),
		})
	}
	return out
}

// checkToolErrors reports each MCP tool whose upstream errors cannot all be
// registered at once.
//
// One finding per TOOL, not per call, and the asymmetry with every other
// per-call check here is the point: the conflict is a property of the tool's
// single OnToolError registration, so naming each call separately would report
// one defect several times and imply each could be dealt with alone.
//
// The calls are still listed inside the message, because the reader's next
// question is always which turns disagreed. Folded.ErrorResults is in
// transcript order and NonUniformErrorTools is sorted, so the output is
// deterministic.
//
// Nothing is reported for a call the GATE refused. Fold classifies those out
// before they reach ErrorResults at all — the tool never ran, and the replay's
// own gate refuses it again from the fixture's seed and configuration.
func checkToolErrors(in SelfCheckInput) []Finding {
	var out []Finding
	for _, er := range in.Folded.ErrorResults {
		if !er.DeniedInLog {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodeUnprovenErrorOrigin,
			Message: fmt.Sprintf("tool call %q (%s, server-side %q) at turn %d came back an error, and the authz "+
				"log ALSO denied that call — but with a different message, so the capture could not prove "+
				"whether the platform refused it or the upstream failed. NOTHING was emitted for it: no "+
				"toolErrors entry and no toolOutputs entry. Canning it would let a REGRESSED gate pass — the "+
				"body is very likely the gate's own refusal text, the step's lastToolResultContains was "+
				"derived from those same bytes, so a gate that stopped refusing would reach the stub, be "+
				"served that text back, and satisfy the assertion meant to catch it. With nothing "+
				"registered, that regression instead fails the step by name. Check that the step still "+
				"proves what you wanted before relying on it.",
				er.Tool, er.UseID, er.Server, er.TurnIndex),
		})
	}
	for _, server := range in.Folded.NonUniformErrorTools {
		var calls []string
		for _, er := range in.Folded.ErrorResults {
			if er.Server == server {
				calls = append(calls, fmt.Sprintf("%s at turn %d", er.UseID, er.TurnIndex))
			}
		}
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeToolErrorNotUniform,
			Message: fmt.Sprintf("tool %q did not fail the same way on every call (upstream errors: %s), so "+
				"its calls cannot all be registered: toolErrors reaches the fake MCP server through "+
				"MCPStub.OnToolError, which is keyed by tool NAME, wins over every canned result, and is "+
				"not counted — so one tool cannot error on one call and succeed on another, nor error "+
				"twice with different text. Replaying this session would answer every call the same way. "+
				"Closing the gap needs a per-call error variant inside toolOutputSequence.",
				server, strings.Join(calls, ", ")),
		})
	}
	return out
}

// checkToolCalls walks the transcript's own tool_use blocks — the fold's output
// cannot answer either question here, because a call the fold emitted no output
// for leaves no trace in Folded at all.
func checkToolCalls(in SelfCheckInput) []Finding {
	prefixes := slices.Sorted(maps.Keys(in.DeclaredTools))
	sandboxPrefixes := slices.Sorted(maps.Keys(in.SandboxTools))
	meta := make(map[string]bool, len(in.MetaTools))
	for _, name := range in.MetaTools {
		meta[name] = true
	}

	var out []Finding
	reported := map[string]bool{}
	for _, t := range sortedTurns(in.Records.Turns) {
		if t.Role != "assistant" || t.Refused {
			continue
		}
		for _, b := range t.Content {
			if b.Type != "tool_use" || b.ToolUse == nil {
				continue
			}
			// respond_to_user is answered here and NOWHERE below: it is known
			// to run for real without a caller listing it in MetaTools, since
			// the fold gives it dedicated handling. Falling through to the
			// MetaTools lookup instead would flag every capture that ever
			// answered a person.
			//
			// It used to carry one exposure of its own — an `attached`
			// artifact the bt.ReplyPart.Text shorthand could not carry — and
			// no longer does: foldReply emits the full tool_use for any
			// respond_to_user the shorthand cannot express whole.
			if b.ToolUse.Name == respondToUserToolName {
				continue
			}
			if isMCP, _ := splitMCPName(b.ToolUse.Name, prefixes); isMCP {
				continue
			}
			if meta[b.ToolUse.Name] || reported[b.ToolUse.Name] {
				continue
			}
			// A SANDBOX name resolves against the class's own toolBundles. Two
			// outcomes, and they are different failures: the prefix resolving
			// but yielding no class tool means the capture never gathered the
			// manifests that would declare it, which checkSandboxTools reports
			// with the CRs named. Only a name matching nothing at all lands
			// below.
			if isSandbox, classTool := splitSandboxName(b.ToolUse.Name, sandboxPrefixes); isSandbox {
				reported[b.ToolUse.Name] = true
				prefix := sandboxPrefixOf(b.ToolUse.Name, sandboxPrefixes)
				if slices.Contains(in.SandboxTools[prefix], classTool) {
					continue
				}
				out = append(out, Finding{
					Severity: SeverityHard,
					Code:     CodeSandboxToolUnresolved,
					Message: fmt.Sprintf("tool call %q (first at turn %d) names declared toolBundle %q, but the "+
						"capture resolved that bundle to class tools %v — not %q. A sandbox tool exists at "+
						"replay only if the emitted fixture declares the SpiceboxClass tool it is synthesized "+
						"from, so the replayed class will not offer this one and the scripted call comes back "+
						"as an unknown tool. Gather the SpiceboxClass named by the bundle and the "+
						"SpiceboxToolspec it lists (a toolspec is matched to a class tool by the toolspec's "+
						"TOOLKIT name, not by its own).",
						b.ToolUse.Name, t.Index, prefix, in.SandboxTools[prefix], classTool),
				})
				continue
			}
			reported[b.ToolUse.Name] = true
			out = append(out, Finding{
				Severity: SeverityHard,
				Code:     CodeUnroutableToolCall,
				Message: fmt.Sprintf("tool call %q (first at turn %d) resolved to no declared MCP server, no "+
					"resolved sidecar toolbox and no declared toolBundle, and is not a meta tool, so the fold "+
					"emitted no output for it and the replay would diverge here. Every transport a bundle can "+
					"replay is named by the AgentClass or by the session's resolved status, so a name matching "+
					"none of them means the capture did not gather what declares it.",
					b.ToolUse.Name, t.Index),
			})
		}
	}
	return out
}

// checkSandboxResults reports the recorded sandbox results the fold could not
// take at face value. Both lists come from the fold, which is the only place
// that saw the raw result text.
func checkSandboxResults(in SelfCheckInput) []Finding {
	var out []Finding
	for _, r := range in.Folded.UnreplayableSandboxTools {
		if r.Errored {
			out = append(out, Finding{
				Severity: SeverityHard,
				Code:     CodeUnreplayableSandboxResult,
				Message: fmt.Sprintf("sandbox tool %q returned a FAILED result the capture cannot express. A "+
					"failed call composes \"ToolCall failed: <reason> — <message>\", a summary of the "+
					"ToolCall's terminal CONDITION with no stdout in it, and the only lever a replayed "+
					"ToolCall has is the process exit code — which reaches exactly one condition, "+
					"NonZeroExit with \"exit code N\". That one IS captured. This result is something "+
					"else: a Timeout or a Cancellation (conditions our own watchdog writes), an exec error "+
					"the process could not be run at all for, an exit code of 0, or a stderr tail the tool "+
					"TRUNCATED, whose original bytes are gone. None of them can be re-composed from an "+
					"exit code, and canning the recorded text instead would hand the model a summary the "+
					"replay never produced.", r.Tool),
			})
		}
		if r.Succeeded {
			out = append(out, Finding{
				Severity: SeverityHard,
				Code:     CodeUnreplayableSandboxResult,
				Message: fmt.Sprintf("sandbox tool %q returned a SUCCESSFUL result the capture cannot express: "+
					"it matched neither the ordinary process composition (stdout plus the tool's own "+
					"\"[exit=…; artifacts: …]\" trailer), nor a secret-output diversion, nor a STREAMING "+
					"toolkit's composed result the bundle can serve back. An ordinary streaming result IS "+
					"carried now, verbatim; the two this refuses are an IDLE exit, whose Terminal flag ends "+
					"the turn so that serving it as an ordinary result would let the replay run on past "+
					"where the recording stopped, and a result larger than %d bytes, which the replayed "+
					"tool's bounded stdout tail would truncate at the head so the assertion derived from it "+
					"could never match.", r.Tool, bt.StreamResultBudgetBytes),
			})
		}
	}
	out = append(out, checkStreamResults(in)...)
	for _, name := range in.Folded.SecretOutputTools {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodeSecretOutputRecorded,
			Message: fmt.Sprintf("sandbox tool %q produced a SECRET OUTPUT, so what this bundle records as its "+
				"stdout is the diversion's description line, not necessarily what the process printed. The "+
				"value itself is absent from the transcript by construction — the runner diverts it before the "+
				"tool_result block is built — so nothing sensitive reaches the bundle. For a `file:`-sourced "+
				"output the description IS the producer's real stdout and the round trip is faithful; for a "+
				"`stdout:`-sourced one the process's own stdout WAS the secret and is not recoverable from any "+
				"record, so the recorded value is the toolspec's description text. RewriteFixture drops "+
				"spec.secretOutput, so the replayed call composes an ordinary result from it either way.",
				name),
		})
	}
	return out
}

// checkStreamResults reports every sandbox tool whose recorded STREAMING result
// this bundle serves back instead of re-composing, and refuses the one shape
// the replay cannot route.
//
// The warning is not a hedge. A capture is evidence, and what a green steel
// bundle is evidence OF differs between a tool whose result the code under test
// rebuilt and one whose result the bundle handed over — so the difference is
// stated rather than left to a reader who would have to know the streaming
// composition by sight to spot it in the emitted JSON.
//
// The collision is hard for a mechanical reason the warning has no bearing on:
// one toolBundle is one sandbox pod, the fake exec binder's streaming half is
// keyed by pod and receives no argv, so two recorded streams on one pod cannot
// be told apart at replay. Prefix-matched against the fixture's own
// bundle-to-class-tool map rather than by splitting the name on "_", which
// would guess wrong for any class tool whose name contains one.
func checkStreamResults(in SelfCheckInput) []Finding {
	if len(in.Folded.StreamResultTools) == 0 {
		return nil
	}
	var out []Finding
	byPrefix := map[string][]string{}
	prefixes := slices.Sorted(maps.Keys(in.SandboxTools))
	for _, name := range in.Folded.StreamResultTools {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodeStreamResultServedVerbatim,
			Message: fmt.Sprintf("sandbox tool %q is a STREAMING or INTERACTIVE toolkit, and this bundle "+
				"carries the result text it produced and serves that text back as the toolkit's own output. "+
				"The stream-composition path is therefore NOT covered by this bundle: the toolkit emits "+
				"nothing its stream parser recognizes at replay, so the parse, the terminal-result decode "+
				"and the \"(<duration>, $<cost>)\" header derived from them are never exercised — the "+
				"replayed result carries a regenerated \"(exit N)\" header above the recorded one instead. "+
				"The rest of the path still runs for real (the ToolCall, the streaming gateway, the bridge, "+
				"and the tool's own no-terminal-result fallback). Nothing durable holds the toolkit's wire "+
				"stream, so replaying the recorded result is the only reproduction available; this finding "+
				"is what keeps that legible rather than silent.", name),
		})
		for _, p := range prefixes {
			if classTool, ok := strings.CutPrefix(name, p+"_"); ok && slices.Contains(in.SandboxTools[p], classTool) {
				byPrefix[p] = append(byPrefix[p], name)
				break
			}
		}
	}
	for _, p := range slices.Sorted(maps.Keys(byPrefix)) {
		if len(byPrefix[p]) < 2 {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeStreamResultCollision,
			Message: fmt.Sprintf("toolBundle %q recorded a streaming result for more than one of its tools "+
				"(%s), and one toolBundle is one sandbox pod. The fake exec binder's streaming half is "+
				"keyed by pod and hands its driver no argv — the request-inspecting responder that routes "+
				"an ordinary sandbox call by command line has no streaming counterpart — so the pod would "+
				"answer both tools with whichever stream was registered, and the bundle would replay green "+
				"with one tool served the other's output.", p, strings.Join(byPrefix[p], ", ")),
		})
	}
	return out
}

// checkMetaReplies raises both findings about a canned META tool reply: the
// coverage the canning gives up, and a tool whose calls the one-body format
// cannot represent.
//
// Driven off Folded rather than off the assembled bundle, the same input every
// other fold-derived check reads, so a finding exists for a tool the assembly
// step later drops as well as for one it keeps.
func checkMetaReplies(in SelfCheckInput) []Finding {
	if len(in.Folded.MetaToolReplies) == 0 {
		return nil
	}
	var out []Finding
	for _, name := range slices.Sorted(maps.Keys(in.Folded.MetaToolReplies)) {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodeMetaReplyCanned,
			Message: fmt.Sprintf("meta tool %q is CANNED in this bundle: its recorded reply is carried in "+
				"metaToolReplies and handed straight back, so the tool's own body never runs at replay. "+
				"What stops being covered is that body — its argument parsing and refusals, the query it "+
				"issues, the per-datum filtering of what comes back, and how it renders the result the "+
				"model is handed. A regression in any of those is served the recorded answer and passes. "+
				"What DOES still run: the capability that offers the tool, the recorded catalog it appears "+
				"in, the system prompt and introspect_tool index over it, the authz check and plan gate "+
				"against its declared permission, the approval flow, and every pipeline hook — a refused "+
				"call returns before Execute and never reaches the canned bytes. Canning is the last of "+
				"the three answers a replay has (reproduce, seed, can); it is taken here because %q reads "+
				"state the fixture has no way to hold. assert.toolsCalled names %q, which the bundle is "+
				"refused without.", name, name, name),
		})
	}
	for _, name := range in.Folded.NonUniformMetaTools {
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeMetaReplyNotUniform,
			Message: fmt.Sprintf("meta tool %q did not answer the same way on every call — two successes "+
				"with different bodies, or a success beside an error. metaToolReplies is ONE body per tool "+
				"name, served on every call, so there is no spelling for that: canning the first body "+
				"would serve it to a call that recorded something else, and the step derived from that "+
				"other call would fail downstream naming the tool rather than this capture. Capturing "+
				"such a session needs a per-call sequence for meta replies, which does not exist.", name),
		})
	}
	return out
}

// checkSteps reports a bundle step that asserts nothing.
//
// Produced by two consecutive assistant turns, or by a capture opening on an
// assistant turn: the fold has no pending Expect to hand the step, so it gets
// the zero value and the driver's checkExpect skips every comparison.
func checkSteps(in SelfCheckInput) []Finding {
	var out []Finding
	for i, step := range in.Folded.LLM {
		if !expectAssertsNothing(step.Expect) {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeEmptyExpect,
			Message: fmt.Sprintf("bundle step %d has an empty expect block, so checkExpect asserts NOTHING "+
				"there and that step of the positional replay is unguarded — a run that took a different "+
				"path would still be handed this step's reply. Two consecutive assistant turns, or a "+
				"capture opening on one, produce this.", i),
		})
	}
	return out
}

// expectAssertsNothing reports whether a step's Expect makes no claim that the
// driver's checkExpect would compare.
//
// Written field by field rather than compared against the zero value, for two
// reasons. Expect now carries a map, so it is no longer comparable at all. And
// equality would have counted Expect.Capture as an assertion: a capture BINDS a
// value for a later step to interpolate and asserts nothing about this one, so
// a step carrying only a capture is still unguarded and must still be reported.
//
// A new assertion field added to Expect and not added here reads as "asserts
// nothing" and raises a finding that is not real. That is the safe direction:
// a false hard finding blocks the capture loudly, where the reverse would let
// an unguarded step through in silence.
func expectAssertsNothing(e bt.Expect) bool {
	return e.UserTextContains == "" &&
		e.LastToolResult == "" &&
		e.LastToolResultContains == "" &&
		e.LastToolResultNotContains == "" &&
		e.LastToolResultIsError == nil &&
		len(e.ToolResultCounts) == 0 &&
		e.ToolOffered == "" &&
		e.ToolNotOffered == ""
}

// checkSecrets scans the FINAL bytes of EVERY emitted file — bundle.json, the
// golden trace, each fixture manifest and the trigger payload — for every live
// value the capture read.
//
// Every file, and that is the fix this check most needed. It used to scan
// SelfCheckInput.Files, which is one of the four surfaces WriteResult writes.
// The other three went out unread, and the one where a leaked credential is
// most likely to be was among them: bundle.json carries whole tool RESULTS, so
// a kubeconfig or an API key an upstream server handed back was written into
// the repo with the scan reporting clean. See SelfCheckInput.Emitted.
//
// Over the bytes rather than the objects deliberately: RewriteFixture rebuilds
// metadata from an allowlist, but this check is the backstop for everything
// that allowlist does not describe — a value surviving in an annotation, a
// comment, or a field added to a CRD after the rewrite table was written.
//
// Every live value EXCEPT the ones their own type declares public. Being read
// out of a Secret is provenance, not sensitivity: an app id and an installation
// id sit in the same Secret as a private key, and the second of those is in the
// body of every webhook GitHub sends, so a capture that treats Secret
// membership as secrecy refuses every triggered GitHub session forever. See
// LiveSecret.Public for why that narrowing is safe and what still catches a
// key declared public in error.
//
// It refuses before it scans when it has nothing to scan for.
//
// TWO of this file's inputs are gathered EXTERNALLY and prove nothing when they
// come back empty, because for each one "there was nothing" and "nobody looked"
// are the same value. Both are therefore gated on an independent fact that says
// the thing being looked for could have existed:
//
//   - LiveSecrets, gated here on ShadowedSecrets — the emitted placeholder
//     Secrets whose NAME the rewrite took from a live reference. RewriteFixture
//     never sees a live Secret's value, so a caller must read them separately,
//     and a placeholder standing in for a named live Secret proves credential
//     material existed to be read. The gate is per Secret: a placeholder the
//     rewrite INVENTED (a class that declares no apiKey because the settings
//     tiers supply its credential) stands in for nothing and demands nothing.
//   - Records.TransformedToolUseIDs, gated in checkTransformedOutputs on
//     GuardConfigured. It is gathered from the guard audits, and a configured
//     tool guard proves a guard was in a position to rewrite output.
//
// Every OTHER check reads a value the fold or the seed derived in-process, so
// an empty one there honestly does mean "nothing wrong". Closing these two
// structurally rather than documenting them is the difference between a
// caller's omission surfacing here and surfacing as a credential in a repo, or
// as a double-transformed output that diverges at replay.
func checkSecrets(in SelfCheckInput) []Finding {
	var out []Finding

	// Gated on what is SCANNABLE, not on whether the slice is populated. The
	// loop below skips every empty Value (see LiveSecret.Value), so a slice
	// whose entries are all empty scans for nothing and reports clean — the
	// same fail-open a nil slice used to produce, wearing a populated field.
	// Reachable, not theoretical: a Secret key present but blank, a read that
	// leaves Value unpopulated, and a partial failure yielding structurally
	// valid entries all land here.
	for _, name := range in.ShadowedSecrets {
		if secretWasRead(in.LiveSecrets, name) {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeSecretCheckSkipped,
			Message: fmt.Sprintf("the leak scan did NOT run for Secret %q: the fixture emits a placeholder "+
				"under that name, so the rewrite substituted material a LIVE Secret holds — but nobody read "+
				"that Secret (%d LiveSecrets entries, none of them %q with a non-empty Value, and none "+
				"recording an EmptyRead of %q; a blank Value alone is skipped, since \"\" matches every "+
				"file). A clean result here would prove nothing. Read that Secret and add one "+
				"SelfCheckInput.LiveSecrets entry per data key — or, if it genuinely holds nothing, one "+
				"EmptyRead entry naming it.",
				name, len(in.LiveSecrets), name+"/<key>", name),
		})
	}
	if bearer, why := unparsableFixtureFile(in.Files); bearer != "" {
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeSecretCheckSkipped,
			Message: fmt.Sprintf("the leak scan cannot be trusted: emitted fixture file %s %s. Neither the "+
				"Secret-bearing gate nor a reader can say what it holds, so a clean scan of it proves "+
				"nothing.", bearer, why),
		})
	}

	// Emitted, not Files: one list holding every surface, so a value living in
	// two of them is reported per file it is in, and a fixture manifest is not
	// reported twice for appearing on both lists.
	for _, f := range in.Emitted {
		for _, s := range in.LiveSecrets {
			if s.Value == "" {
				// "" is a substring of every file. See LiveSecret.Value.
				continue
			}
			if s.Public {
				// A public identifier is not a credential, and this scan is
				// the wrong instrument for it. See LiveSecret.Public — and
				// note the structural scan still reads these same bytes with
				// no gate, so a key wrongly declared public whose value has a
				// credential's SHAPE is still refused there.
				continue
			}
			if !bytes.Contains(f.Bytes, []byte(s.Value)) {
				continue
			}
			out = append(out, Finding{
				Severity: SeverityHard,
				Code:     CodeSecretLeak,
				Message: fmt.Sprintf("emitted file %s contains the live value of secret %s; these files "+
					"are written into a repo, so the capture refuses rather than committing a credential. "+
					"(The value itself is deliberately not reprinted here.)",
					f.Name, s.Name),
			})
		}
	}
	return out
}

// checkStructuralSecrets is the shape half of the secret scan, and it runs
// unconditionally over every emitted byte.
//
// Kept as its own check rather than folded into checkSecrets because the two
// answer different questions and have opposite failure modes. checkSecrets is
// exact and needs an input it can only be GIVEN, which is why it carries a gate
// saying so when the input is missing. This one needs nothing: a clean result
// is a fact about the bytes. Gating it on anything — a flag, a populated
// LiveSecrets, a class setting — would recreate the fail-open the other gate
// exists to close, so it has no gate and no escape hatch. See secretscan.go.
func checkStructuralSecrets(in SelfCheckInput) []Finding {
	return scanStructural(in.Emitted, redactionTokens(in.Bundle))
}

// redactionTokens reads the replacement tokens off the bundle's own capture
// stanza — the strings this capture substituted IN.
//
// Read from the assembled bundle rather than taken as a separate input for the
// reason PlaceholderTools gives: the check must describe the bytes a reader is
// holding, and the bundle is where those substitutions are recorded. A second
// copy passed alongside would be free to disagree with it.
//
// Used by the ENTROPY warning only, never by the hard patterns. See
// entropyFinding.
func redactionTokens(b bt.Bundle) []string {
	if b.Capture == nil {
		return nil
	}
	out := make([]string, 0, len(b.Capture.Redactions))
	for _, r := range b.Capture.Redactions {
		out = append(out, r.Replacement)
	}
	return out
}

// nonTransportTool reports whether an LLM-facing name resolves to NEITHER a
// declared MCP server NOR a declared toolBundle — the two transports whose
// results a bundle CANS, so whose tools exist at replay because the fixture
// declares them.
//
// Whatever is left is served by the runner itself: respond_to_user, which the
// fold handles by name, and the meta tools the capability assembly produces.
// Those exist at replay only if the fixture's own assembly produces them, which
// is what makes this the right partition for both checks below.
//
// It is the same three-way resolution checkToolCalls performs, through the same
// two splitters, so a name classified one way there cannot be classified
// another way here.
func nonTransportTool(in SelfCheckInput, name string) bool {
	if name == respondToUserToolName {
		return false
	}
	if isMCP, _ := splitMCPName(name, slices.Sorted(maps.Keys(in.DeclaredTools))); isMCP {
		return false
	}
	if isSandbox, _ := splitSandboxName(name, slices.Sorted(maps.Keys(in.SandboxTools))); isSandbox {
		return false
	}
	return true
}

// recordedRunnerToolCalls is every runner-served tool the transcript CALLED,
// mapped to the turn it first appeared at.
func recordedRunnerToolCalls(in SelfCheckInput) map[string]int {
	out := map[string]int{}
	for _, t := range sortedTurns(in.Records.Turns) {
		if t.Role != "assistant" || t.Refused {
			continue
		}
		for _, b := range t.Content {
			if b.Type != "tool_use" || b.ToolUse == nil {
				continue
			}
			if _, seen := out[b.ToolUse.Name]; seen {
				continue
			}
			if nonTransportTool(in, b.ToolUse.Name) {
				out[b.ToolUse.Name] = t.Index
			}
		}
	}
	return out
}

// checkFixtureTools compares what the run was OFFERED with what the emitted
// fixture will offer, and refuses the difference.
//
// The recorded catalog is the replay's own assertion — bt.ToolCatalogCheck
// fails the run on a tool that is missing from it — so this check does not
// invent a rule; it moves an existing one to the point a human can act on. The
// tools it can speak about are the ones the RUNNER serves (see
// nonTransportTool): an MCP or sandbox tool exists at replay because the
// fixture declares it, and checkToolCalls and checkSandboxTools already report
// the ways that declaration can be missing.
func checkFixtureTools(in SelfCheckInput) []Finding {
	comparable := len(in.Folded.ToolCatalogs) > 0 || len(recordedRunnerToolCalls(in)) > 0
	if len(in.FixtureTools) == 0 {
		if !comparable {
			// Nothing recorded to compare against: a session captured before
			// the tool_catalog Kind shipped that also called no runner-served
			// tool. There is no claim to check and none is made.
			return nil
		}
		return []Finding{{
			Severity: SeverityHard,
			Code:     CodeFixtureToolsNotComputed,
			Message: "the capture recorded what tools this run was offered but was given no prediction of what " +
				"the EMITTED fixture will offer, so nothing compared the two. The comparison is what catches a " +
				"rewrite that removes a tool source — the Channel rewrite above all, which replaces the kind " +
				"every channel-sourced meta tool was assembled against. Supply SelfCheckInput.FixtureTools by " +
				"assembling the session's capabilities a second time against the Channel, Secret and kind " +
				"steelthread.ReplayChannel returns.",
		}}
	}

	offered := make(map[string]bool, len(in.FixtureTools))
	for _, t := range in.FixtureTools {
		offered[t.Name] = true
	}

	// One finding naming every missing tool rather than one per tool: they all
	// have the same cause and the same fix, and a reader deciding whether this
	// session can be captured at all wants the whole list at once.
	var missing []string
	seen := map[string]bool{}
	for _, c := range in.Folded.ToolCatalogs {
		for _, name := range c.Tools {
			if seen[name] || !nonTransportTool(in, name) || offered[name] {
				continue
			}
			seen[name] = true
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return []Finding{{
		Severity: SeverityHard,
		Code:     CodeOfferedToolNotProducible,
		Message: fmt.Sprintf("the recorded run was offered %d tool(s) the emitted fixture will not offer: %s. "+
			"A bundle pins the catalog it recorded and the replay fails on any tool missing from it, so this "+
			"bundle would emit clean and then fail inside the suite, naming the driver instead of the capture. "+
			"The usual cause is the Channel rewrite: every non-trigger Channel becomes kind=fake, and a meta "+
			"tool the live kind contributed has no fake counterpart. There is no fix inside the capture — "+
			"teaching the fake kind to advertise the surface would make the fixture claim something it does "+
			"not have. Capture a session on a channel kind whose fixture form still offers these, or finish "+
			"this bundle by hand.",
			len(missing), strings.Join(missing, ", ")),
	}}
}

// checkExternalSurfaceCalls refuses a transcript that called a tool whose result
// came from the input Channel kind's own PROVIDER rather than from anything the
// fixture can stand up. See CodeExternalSurfaceToolCalled: the limit is
// permanent, and the finding says so.
func checkExternalSurfaceCalls(in SelfCheckInput) []Finding {
	external := map[string]bool{}
	for _, t := range in.FixtureTools {
		if t.ExternalSurface {
			external[t.Name] = true
		}
	}
	if len(external) == 0 {
		return nil
	}

	var called []string
	for name, turn := range recordedRunnerToolCalls(in) {
		if external[name] {
			called = append(called, fmt.Sprintf("%s (first at turn %d)", name, turn))
		}
	}
	if len(called) == 0 {
		return nil
	}
	slices.Sort(called)

	if in.TriggerProviderMissing {
		return []Finding{{
			Severity: SeverityHard,
			Code:     CodeProviderStateNotComputed,
			Message: fmt.Sprintf("the transcript called %d tool(s) that reach the input Channel kind's own "+
				"provider (%s), and nobody supplied that kind's trigger-status reporter, so the values the run "+
				"read back from the provider were never extracted. An empty stand-in seed has two readings — "+
				"'the run observed nothing' and 'nobody looked' — and only the second is a failure of the "+
				"capture. Supply CaptureInput.TriggerProvider from the channel-kind registry.",
				len(called), strings.Join(called, ", ")),
		}}
	}

	// The pull-request read comes FIRST inside the status surface, before the
	// claim, the create or anything else — so a stand-in with no revision to
	// report does not merely answer differently, it refuses outright and every
	// call to the surface errors. Nothing downstream of that is worth checking.
	if in.Bundle.Trigger == nil || in.Bundle.Trigger.HeadSHA == "" {
		return []Finding{{
			Severity: SeverityHard,
			Code:     CodeProviderStateUnseeded,
			Message: fmt.Sprintf("the transcript called %d tool(s) that reach the input Channel kind's own "+
				"provider (%s), and no revision of the triggering resource could be read back out of the "+
				"records. The status surface resolves the commit it answers for by READING that resource "+
				"before it does anything else, so a stand-in with nothing to report refuses every one of "+
				"those calls — the replay fails on the first of them, naming the surface rather than this "+
				"capture. The value lives only inside text the channel kind composed; if the kind's own "+
				"rendering of it changed, its TriggerProviderStateIn has drifted from the formatter it "+
				"inverts.",
				len(called), strings.Join(called, ", ")),
		}}
	}
	return nil
}

// mintShapedID matches this repo's minting shape: a lowercase family prefix and
// the 16 hex digits every crypto/rand mint site emits from 8 random bytes.
//
// Anchored on a non-identifier boundary at each end so a longer name that merely
// ENDS in that shape is not read as a mint — a render handle is
// `ar-<session>-<suffix>`, and a session name ending in hex must not turn one
// into a false positive.
var mintShapedID = regexp.MustCompile(`(^|[^A-Za-z0-9_-])([a-z][a-z0-9]*)-([0-9a-f]{16})($|[^A-Za-z0-9_-])`)

// mintShapedIDsIn returns every generic-mint-shaped id in text, in order.
func mintShapedIDsIn(text string) []string {
	var out []string
	for _, m := range mintShapedID.FindAllStringSubmatch(text, -1) {
		out = append(out, m[2]+"-"+m[3])
	}
	return out
}

// familyShapedIDsIn returns every id in text that a REGISTERED family claims,
// found by decoding the result and walking it with the fold's own matcher.
//
// Reusing mintedIDsIn rather than a second regexp is the whole point: it is the
// function that decides what the capture COLLECTS, so anything it finds is
// something the bundle was able to pin, and anything it misses is something the
// bundle could not have pinned either. Two independent notions of "is this an
// id" would let a value be collectable but unchecked, or checked but
// impossible to satisfy.
//
// A payload that is not JSON yields nothing, which is the ordinary case for
// every non-meta tool; this check only ever reads meta results anyway.
func familyShapedIDsIn(text string) []string {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil
	}
	var body any
	if err := json.Unmarshal([]byte(text[start:end+1]), &body); err != nil {
		return nil
	}
	return mintedIDsIn(body)
}

// checkUnreproducibleIDs refuses a meta tool's recorded result that names an id
// in this repo's minting shape which the bundle does not pin. See
// CodeUnreproducibleID.
//
// It reads the FOLDED expectations rather than the raw transcript, and that is
// the point: Expect.LastToolResultContains is the exact claim the replay will
// check, so an id that reached it is an id a divergence will be reported on. One
// that appears only somewhere the fold dropped costs nothing and is not a
// finding.
func checkUnreproducibleIDs(in SelfCheckInput) []Finding {
	meta := make(map[string]bool, len(in.MetaTools))
	for _, n := range in.MetaTools {
		meta[n] = true
	}
	pinned := map[string]bool{}
	for _, ids := range in.Folded.MintedIDs {
		for _, id := range ids {
			pinned[id] = true
		}
	}

	seen := map[string]bool{}
	var found []string
	for i, step := range in.Folded.LLM {
		if !meta[step.Expect.LastToolResult] || step.Expect.LastToolResultContains == "" {
			continue
		}
		ids := mintShapedIDsIn(step.Expect.LastToolResultContains)
		// A family's OWN shape, found the way the fold collects: decode the
		// result and walk it with the same matcher, so anything collectable is
		// also checkable.
		//
		// Not redundant with the regexp above, and the render handle is why:
		// its shape carries the session name between prefix and tail, which the
		// generic 16-hex rule cannot express and which was listed for a while
		// as a gap this check deliberately did not catch. Running both means a
		// family is covered by whichever rule fits it, and a family that gains
		// a seam still stops firing the moment its ids are pinned.
		ids = append(ids, familyShapedIDsIn(step.Expect.LastToolResultContains)...)
		for _, id := range ids {
			if pinned[id] || seen[id] {
				continue
			}
			seen[id] = true
			found = append(found, fmt.Sprintf("%s (in %s at llm step %d)", id, step.Expect.LastToolResult, i))
		}
	}
	if len(found) == 0 {
		return nil
	}
	slices.Sort(found)
	return []Finding{{
		Severity: SeverityHard,
		Code:     CodeUnreproducibleID,
		Message: fmt.Sprintf("%d identifier(s) in this repo's own minting shape appear in a meta tool's "+
			"recorded result and are not pinned by this bundle: %s. A meta tool runs FOR REAL at replay, "+
			"so everything else in its reply is composed by our code from our inputs — but an id minted "+
			"during the run is not, and the replay mints a different one. The step's expectation was "+
			"derived from the recorded bytes, so this bundle would emit clean and then fail inside the "+
			"suite, naming the driver instead of this capture. Pinning it needs an injectable seam at the "+
			"component's own mint site plus a row in bronzethread's MintedIDFamilies, exactly as the "+
			"operation and artifact families have. Read the WHOLE recorded result before deciding: a "+
			"result carrying one such id often carries other per-run values this shape cannot match.",
			len(found), strings.Join(found, ", ")),
	}}
}

// checkStoodInCalls refuses a transcript that CALLED a tool the fixture can only
// OFFER through a seeded stand-in. See CodeStoodInToolCalled.
//
// The line it draws is between reproducing an offer and reproducing an answer.
// A seeded directory restores the offer exactly — the capability assembly, the
// tool's schema and every one of its refusals are our code and all of it runs —
// but the REPLY is the stand-in kind's own rendering of the resolved value, and
// a stand-in renders it differently from the kind that recorded it. So the offer
// is honest and the answer is not, and a bundle may rest on the first and not
// the second.
func checkStoodInCalls(in SelfCheckInput) []Finding {
	stoodIn := map[string]bool{}
	for _, t := range in.FixtureTools {
		if t.StoodIn {
			stoodIn[t.Name] = true
		}
	}
	if len(stoodIn) == 0 {
		return nil
	}

	var called []string
	for name, turn := range recordedRunnerToolCalls(in) {
		if stoodIn[name] {
			called = append(called, fmt.Sprintf("%s (first at turn %d)", name, turn))
		}
	}
	if len(called) == 0 {
		return nil
	}
	slices.Sort(called)
	return []Finding{{
		Severity: SeverityHard,
		Code:     CodeStoodInToolCalled,
		Message: fmt.Sprintf("the transcript called %d tool(s) the emitted fixture can only OFFER through a "+
			"seeded stand-in, not answer: %s. The fixture rewrite replaced the collaborator behind them — "+
			"every non-trigger Channel becomes kind=fake — and seeding the stand-in restores which values "+
			"exist, so the tool is offered exactly as the run saw it. What it does not restore is the REPLY: "+
			"that is composed by the stand-in kind's own rendering, which differs from the kind that recorded "+
			"it, so the step's expectation was derived from bytes the replay cannot produce. Such a session is "+
			"captured against a channel kind whose fixture form really serves the surface, or finished by "+
			"hand.",
			len(called), strings.Join(called, ", ")),
	}}
}

// checkFixtureCredentials refuses a fixture carrying a credential whose value is
// MINTED rather than stored AND whose type no replay harness stands a minter up
// for. See CodeCredentialNotMintable.
//
// The filter is bt.StandInMintedCredentialTypes and nothing else. A minted type
// on that list is served at replay against the same fixture provider the rest of
// the run is pointed at, so the credential is minted FOR REAL — the App JWT is
// signed, the token exchange happens over HTTP, the broker's expiry rule reads
// the expiry the minter returned. Nothing about the minted-ness is stood in for,
// which is exactly why converting the credential to a stored type would have
// been the wrong fix and standing a minter up is the right one.
func checkFixtureCredentials(in SelfCheckInput) []Finding {
	var unservable []string
	for _, c := range in.MintedCredentials {
		if !slices.Contains(bt.StandInMintedCredentialTypes, c.Type) {
			unservable = append(unservable, c.Label)
		}
	}
	if len(unservable) == 0 {
		return nil
	}
	return []Finding{{
		Severity: SeverityHard,
		Code:     CodeCredentialNotMintable,
		Message: fmt.Sprintf("the emitted fixture declares %d credential(s) whose type mints a fresh value on "+
			"every resolve instead of reading a stored one, and which no replay harness stands a minter up "+
			"for: %s. The rewrite emits a placeholder Secret beside each, which is enough for the replayed "+
			"identity to go Valid and is worth nothing at resolve time: a minted type ignores that Secret and "+
			"calls an external minter. Every tool call drawing on the credential fails at dispatch with the "+
			"identity reporting healthy. Converting it to a stored type is NOT the fix — whether a credential "+
			"is minted is read by the broker's expiry rule and by identity-scoped token grants, so the bundle "+
			"would then be evidence about a gate the session never ran. The fix is a stand-in minter in the "+
			"harness plus the type's name in bronzethread's StandInMintedCredentialTypes; the types served "+
			"today are %v.",
			len(unservable), strings.Join(unservable, ", "), bt.StandInMintedCredentialTypes),
	}}
}

// checkSkills refuses a capture whose class opts into a skill the fixture does
// not carry, and warns about a captured skill whose bundle archive it could not
// carry.
//
// ONE hard finding naming every uncaptured skill, not one per skill, and the
// asymmetry with checkUnmappedTurns is deliberate. There, each hole is
// independently fixable and collapsing them makes a reader fix the first and
// re-run to find the second. Here one cause covers all of them — the refs that
// resolved to no namespaced Skill — so N findings would be N copies of one
// sentence.
//
// The comparison is between what the CLASS opted into and what the REWRITE
// emitted, never between the class and itself: see SelfCheckInput.CapturedSkills.
func checkSkills(in SelfCheckInput) []Finding {
	var out []Finding

	captured := make(map[string]bool, len(in.CapturedSkills))
	for _, c := range in.CapturedSkills {
		captured[c] = true
	}
	var missing []string
	for _, ref := range in.ClassSkills {
		if !captured[ref] {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeSkillsNotCaptured,
			Message: fmt.Sprintf("the captured AgentClass opts into %d skill(s) %v that this capture emits no "+
				"Skill CR for: each one resolved to no Skill in the session's namespace. Either the skill is "+
				"backed by a cluster-scoped ClusterSkill, which this capture does not gather, or the Skill the "+
				"class was validated against has been deleted or renamed since. The replayed AgentClass would "+
				"park at Valid=False/AgentClassSkillMissing, the session reconciler refuses to spawn against an "+
				"invalid class, and the run would fail at the driver's readiness barrier rather than at any step "+
				"the bundle describes.",
				len(missing), missing),
		})
	}

	if len(in.SkillsWithoutBundle) > 0 {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodeSkillBundleNotStaged,
			Message: fmt.Sprintf("%d captured skill(s) %v ship a bundle archive — the scripts and assets a "+
				"sandbox-targeted skill is staged to disk with — and this bundle carries only their SKILL.md. "+
				"The archive's bytes live in the operator's skillbundle store keyed by digest, in no CR and in "+
				"no durable record this capture reads, so there is nothing here to gather. The replay stages "+
				"the composed SKILL.md alone, which is the same degradation the operator's own staging path "+
				"applies to an uncached bundle; anything in the transcript that depended on one of those "+
				"scripts running is NOT reproduced.",
				len(in.SkillsWithoutBundle), in.SkillsWithoutBundle),
		})
	}

	for _, e := range in.SkillElisions {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodeSkillContentElided,
			Message: fmt.Sprintf("an operator-declared skill elision rewrote %d SkillSource(s) and %d Skill(s) "+
				"onto the stand-in authority %q and dropped %d byte(s) of their content — bodies, descriptions, "+
				"repo instructions and the non-identity frontmatter. The emitted Skill manifests therefore say "+
				"something the session's skills did not. Every elided skill's AgentClass link targets the "+
				"sandbox, which is enforced rather than assumed: eliding a skill the agent could see is refused "+
				"outright, because its description reaches the composed system prompt. The replay depends on "+
				"these skills existing, resolving and passing their provenance gate, and on nothing they said.",
				e.Sources, e.Skills, e.Authority, e.ElidedBytes),
		})
	}

	return out
}

func checkSeed(in SelfCheckInput) []Finding {
	var out []Finding
	for _, key := range in.Seed.Unseeded {
		out = append(out, Finding{
			Severity: SeverityHard,
			Code:     CodeUnseededAllow,
			Message: fmt.Sprintf("the session was ALLOWED %s but the derived seed contains no tuple that grants "+
				"it: the grant came from somewhere this derivation cannot see, so the replay fixture will "+
				"deny where the real run allowed. %s", key, unseededSlotHint(in.Seed.DeclaredSlots)),
		})
	}
	return out
}

// unseededSlotHint names WHY this key survived the slot exemption, which is the
// one thing a reader needs and the key itself cannot say.
//
// Same code and same severity either way — the consequence is identical, and a
// second finding code would suggest two different defects where there is one.
// What differs is the reader's NEXT MOVE. A class that declares slots and does
// not declare this pair has been asked and answered: the grant is genuinely
// pre-existing state, go find the relationship. A derivation that consulted no
// declaration at all cannot rule the slot case out, and the first thing to
// check is whether the class has an AgentSessionGrants — a capture taken
// against an unreconciled class would otherwise send someone hunting a tuple
// that was never missing.
func unseededSlotHint(declaredSlots int) string {
	if declaredSlots == 0 {
		return "No declared authz slot was consulted for this session (the class declares none, or its " +
			"AgentSessionGrants was not gathered), so a slot binding collected at session end has not been " +
			"ruled out as the explanation — confirm the class's AgentSessionGrants before hunting a " +
			"pre-existing relationship."
	}
	return fmt.Sprintf("The class declares %d authz slot(s) and this (resourceType, permission) pair is not "+
		"one of them, so a collected slot binding does not explain it: the grant is pre-existing state the "+
		"fixture has to seed.", declaredSlots)
}

func checkTrigger(in SelfCheckInput) []Finding {
	if in.Records.Trigger != nil {
		var out []Finding
		if in.Records.Trigger.Truncated {
			out = append(out, Finding{
				Severity: SeverityHard,
				Code:     CodeTruncatedTrigger,
				Message: fmt.Sprintf("the recorded trigger delivery (kind %q, event %q, channel key %q) was "+
					"TRUNCATED, and a body cut mid-token cannot be re-signed into a delivery that passes HMAC "+
					"verification; the bundle would fail at the webhook route with nothing pointing back here.",
					in.Records.Trigger.Kind, in.Records.Trigger.Event, in.Records.Trigger.ChannelKey),
			})
		}
		if in.TriggerChannel == "" {
			out = append(out, Finding{
				Severity: SeverityHard,
				Code:     CodeTriggerChannelUnknown,
				Message: fmt.Sprintf("a trigger delivery (kind %q, event %q, channel key %q) opened this session, "+
					"but no input Channel was identified for it. The bundle can neither replay the delivery — "+
					"bt.Trigger names the Channel the driver signs with — nor suppress the SYNTHESIZED prompt "+
					"the runner built from it, so it would send that prompt as a message a person typed.",
					in.Records.Trigger.Kind, in.Records.Trigger.Event, in.Records.Trigger.ChannelKey),
			})
		}
		return out
	}
	if in.TriggerChannel == "" {
		return nil
	}
	return []Finding{{
		Severity: SeverityWarn,
		Code:     CodeNoTriggerRecord,
		Message: fmt.Sprintf("the session opened on trigger channel %q but recorded no trigger_delivery, so the "+
			"bundle falls back to userTurns and cannot exercise the signed-webhook path the session actually "+
			"took. A session captured before that Kind shipped is still worth emitting.", in.TriggerChannel),
	}}
}

// checkReplies reports a capture that answered nobody.
//
// Through replyText rather than reading bt.ReplyPart.Text directly, and the
// distinction is not stylistic: a reply that DELIVERED an artifact folds to a
// full tool_use, so a reader looking only at the shorthand sees no reply at all
// and tells a session that plainly answered a person that it "can assert
// nothing about what the agent said." That is wrong in the direction that reads
// as evidence of a thin capture. deriveReplies is the other consumer of the
// same fact, and the two must not drift.
func checkReplies(in SelfCheckInput) []Finding {
	for _, step := range in.Folded.LLM {
		for _, part := range step.Reply {
			if replyText(part) != "" {
				return nil
			}
		}
	}
	return []Finding{{
		Severity: SeverityWarn,
		Code:     CodeNoAgentReply,
		Message: fmt.Sprintf("the capture found no respond_to_user reply across %d assistant step(s), so the "+
			"bundle can assert nothing about what the agent said. A session whose evidence is its "+
			"authorization trace is legitimately in this state.", len(in.Folded.LLM)),
	}}
}

// checkBundle runs the replay driver's OWN structural preconditions over the
// assembled bundle.
//
// It is the last check on purpose: every other check reports a fidelity gap in
// a bundle that would at least LOAD, and this one reports that it would not
// load at all. Delegating to bt.Bundle.Validate rather than restating the rules
// here is the whole point — a rule stated twice is a rule that drifts, and the
// drift is silent in the direction that matters (a capture reporting clean and
// a loader refusing the file).
func checkBundle(in SelfCheckInput) []Finding {
	err := in.Bundle.Validate()
	if err == nil {
		return nil
	}
	return []Finding{{
		Severity: SeverityHard,
		Code:     CodeBundleInvalid,
		Message: fmt.Sprintf("the assembled bundle is not loadable by the replay driver: %s. "+
			"Emitting it would put a file on disk that every suite reading it refuses, with nothing "+
			"pointing back at the capture that wrote it.", err),
	}}
}

func checkApprovals(in SelfCheckInput) []Finding {
	var out []Finding
	for _, id := range slices.Sorted(slices.Values(in.UndeterminedApprovals)) {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Code:     CodeUndeterminedApprovalCategory,
			Message: fmt.Sprintf("approval for tool call %q was DECIDED, but no durable record names the "+
				"interaction category that asked for it, so the capture cannot put it in autoApprove or "+
				"autoDeny. Add it by hand after looking at what it was; without it the replay blocks until "+
				"the class timeout and reads as a hang.", id),
		})
	}
	return out
}

// scannableSecrets counts the entries the leak scan can actually compare
// against — those carrying a non-empty Value. It is deliberately the same
// predicate the scan loop skips on, so "the gate says the check ran" and "the
// check compared something" cannot drift apart.
func scannableSecrets(secrets []LiveSecret) int {
	var n int
	for _, s := range secrets {
		if s.Value != "" {
			n++
		}
	}
	return n
}

// secretWasRead reports whether the capture read ONE named Secret — either
// because it yielded at least one scannable value, or because the caller
// recorded that it read it and found nothing (LiveSecret.EmptyRead).
//
// LiveSecret.Name is "<secret name>/<data key>", one entry per key, so a
// Secret's value entries are its name followed by a slash. Matched on that
// prefix rather than on equality: which keys a Secret holds is not something the
// rewrite knows, and requiring a caller to name them would move the decision
// about what "read this Secret" means out of the reader that actually read it.
func secretWasRead(secrets []LiveSecret, secretName string) bool {
	prefix := secretName + "/"
	for _, s := range secrets {
		if s.EmptyRead && s.Name == secretName {
			return true
		}
		if s.Value != "" && strings.HasPrefix(s.Name, prefix) {
			return true
		}
	}
	return false
}

// unparsableFixtureFile names the first emitted fixture file that could not be
// read at all, and WHY — a clause completing "emitted fixture file <name> …".
// Returns ("", "") when every file parsed.
//
// WHICH Secrets the fixture emits is answered by RewriteResult.ShadowedSecrets,
// by the rewrite that emitted them, so this no longer sniffs documents for
// `kind: Secret`. What it still answers is the question ShadowedSecrets cannot:
// a file nobody can parse might hold anything, so a clean scan of it proves
// nothing about it, and the capture must say so rather than pass.
//
// The parse error is carried into the reason rather than dropped: it is the
// only account of why the file was unreadable, and this is the one place in
// this file where an error would otherwise vanish.
func unparsableFixtureFile(files []FixtureFile) (name, why string) {
	for _, f := range files {
		// marshalDocs joins documents with "---\n" after a yaml.Marshal that
		// always ends in a newline, so this is the exact separator it wrote.
		for _, doc := range bytes.Split(f.YAML, []byte("\n---\n")) {
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}
			var head struct {
				Kind string `json:"kind"`
			}
			if err := yaml.Unmarshal(doc, &head); err != nil {
				return f.Name, fmt.Sprintf("could not be parsed (%v), so whether it holds credential material is unknown", err)
			}
		}
	}
	return "", ""
}

// sortedTurns orders a clone by Index, matching Fold, so a caller that gathered
// turns off a query with no ordering guarantee gets the same findings Fold's
// own walk would justify.
func sortedTurns(turns []memory.Turn) []memory.Turn {
	out := slices.Clone(turns)
	slices.SortStableFunc(out, func(a, b memory.Turn) int { return a.Index - b.Index })
	return out
}
