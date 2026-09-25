// Package bronzethread runs whole-session tests from an AUTHORED transcript.
//
// A bundle pins the two non-deterministic halves of a session — what the LLM
// said and what stateful tools returned — and lets everything else run for
// real: the operator, the runner loop, the hook pipeline, memory, authz.
//
// # Bronze vs steel
//
// The bundle format here is the format a future `steelthread:capture` will
// emit. The ONLY difference is provenance: a bronzethread transcript is
// authored (an LLM writing what a model plausibly would emit), a steelthread
// one is captured from a real session. Same driver, same divergence contract,
// same disposition rules — so a captured bundle drops in unchanged.
//
// # What a green bundle does and does not prove
//
// It proves the SYSTEM handles this interaction correctly. It does NOT show a
// real model would produce the interaction. That distinction is load-bearing:
// a green bronzethread suite is evidence about code, never about model
// behavior, and must not be read as data for a decision that is gated on the
// latter.
package bronzethread

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Bundle is one whole-session scenario.
type Bundle struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// AgentDir is the fixture the harness boots, relative to the test package.
	AgentDir string `json:"agentDir"`

	// AgentClass is the class the fixture defines, awaited before the run.
	AgentClass string `json:"agentClass"`

	// SeedUserPreferences pre-seeds one or more users' SAVED preference
	// values before the run starts, writing directly into the user's own
	// memory scope through the harness's operator facade
	// (memory.SystemContext — the same door pkg/memory/httpsrv/preferences.go's
	// commit route writes through).
	//
	// The only way an authored bundle can put a value in the `source: user`
	// layer at all: set_preference gates every save behind a human confirm
	// round-trip a scripted transcript has no way to play. Without this a
	// bundle asserting on get_preferences' resolution for a named user could
	// only ever observe the class default or an admin global — never the
	// layer subjectresolve's whole reason for existing is to expose.
	SeedUserPreferences []SeedUserPreference `json:"seedUserPreferences,omitempty"`

	// SeedRelationships pre-seeds SpiceDB relationships before the run
	// starts, through the harness's real SpiceDB (test/e2e's WriteRel).
	//
	// For the one relationship this harness has no other way to produce:
	// a resource's sole_user. In production it is DERIVED, level-triggered,
	// by the useridentity reconciler watching session membership — a
	// controller this harness does not register — so a bundle exercising
	// subjectresolve's generic "<type>:<id>" fallback (or trigger-author's
	// recursion into one) has to state the durable edge directly.
	SeedRelationships []SeedRelationship `json:"seedRelationships,omitempty"`

	// ExtraManifests are per-scenario overrides. NOTE these REPLACE the object
	// rather than patching it, so an override must restate the whole spec — a
	// partial one silently drops required fields and the class fails Valid.
	ExtraManifests []string `json:"extraManifests,omitempty"`

	// ToolOutputs maps a SERVER-side tool name to its canned result. These are
	// the stateful, non-deterministic outputs steelthread would have recorded.
	//
	// Server-side, not LLM-facing: an MCP tool is registered as "list_companies"
	// but called as "<server>_list_companies". Getting that backwards fails as
	// "unknown tool", not as a gate error.
	//
	// EVERY tool the MCPServer declares needs an entry, even one the transcript
	// never calls: the fixture's allowlist is validated at class-admission time,
	// and a missing handler surfaces as AgentClassMCPServerInvalid/AllowlistDrift
	// rather than as anything mentioning the tool.
	//
	// # A SANDBOX tool's entry lives here too
	//
	// A bundle replays BLACK BOXES: the recorded call goes in, the recorded
	// result comes back, and HOW the tool produced it — an MCP server, a
	// sidecar, a `kubectl exec` into a sandbox pod — is transport plumbing the
	// bundle has no business reproducing. So a sandbox tool's recorded output is
	// an entry in this same map, keyed by the name the SpiceboxClass gives the
	// tool in its `spec.tools[]` catalog (which is also the LLM-facing name's
	// suffix, "<bundle>_<tool>", exactly as an MCP server's prefix works).
	//
	// The VALUE is what differs, and it is what the driver classifies on: a
	// sandbox entry is a SandboxOutput object ({"stdout": …}), because a
	// sandbox tool answers with process streams and an exit code rather than
	// with a JSON-RPC result. The driver routes by NAME, not by shape — a name
	// the fixture's SpiceboxClass declares as a tool goes to the fake exec
	// binder, everything else to the fake MCP server.
	ToolOutputs map[string]json.RawMessage `json:"toolOutputs,omitempty"`

	// ToolOutputSequence is ToolOutputs for a tool whose successive calls
	// returned DIFFERENT results.
	//
	// Routine in a capture, rare in an authored bundle — which is why the
	// format needed it only once bundles started being captured. A live session
	// that listed a collection, mutated it, and listed it again cannot be
	// expressed by a name->one-value map at all, even though the transcript
	// holds both results.
	//
	// Mutually exclusive with ToolOutputs PER TOOL NAME: a tool named in both
	// is a bundle error, not a merge, because there is no defensible order
	// between them.
	ToolOutputSequence map[string][]json.RawMessage `json:"toolOutputSequence,omitempty"`

	// ToolErrors maps a SERVER-side tool name to the JSON-RPC error the fake
	// MCP server answers its calls with, instead of a result.
	//
	// This is the ONLY way a bundle says "the upstream itself failed". It is
	// distinct from a call the platform REFUSED — an authz denial, a plan-gate
	// refusal — which never reaches the server at all (see
	// pkg/agent/runner/loop_dispatch.go: "a denied tool never reaches the
	// sandbox or MCP server") and which the replay's own gate regenerates from
	// the fixture. Canning a refusal here would make the stub answer a call the
	// gate already stopped.
	//
	// Registered through MCPStub.OnToolError, which WINS over OnTool for the
	// same name and is not counted, so it applies to every call of that tool.
	// Two consequences a bundle author has to live with:
	//
	//   - A tool named here still needs a ToolOutputs entry, because the stub
	//     builds its tools/list from the handlers registered through OnTool and
	//     a declared tool missing from that list fails class admission as
	//     AgentClassMCPServerInvalid/AllowlistDrift. The entry's value is never
	//     served; the error wins.
	//   - A tool cannot error on one call and succeed on another. There is no
	//     per-call variant, and ToolOutputSequence cannot interleave with this
	//     map — pairing the two is refused by ValidateToolOutputs.
	ToolErrors map[string]ToolError `json:"toolErrors,omitempty"`

	// Capture records that this bundle was TAKEN from a real session rather
	// than authored. Its presence is the bronze/steel distinction.
	//
	// Load-bearing rather than decorative: a green bundle with no Capture
	// proves the SYSTEM handles the interaction; one WITH a Capture also shows
	// a real model produced it, at least once. Those are different claims, and
	// nothing else in the format distinguishes them.
	Capture *Capture `json:"capture,omitempty"`

	// DefaultUser is the channel identity the turns are sent as. Empty keeps the
	// harness default.
	//
	// A scenario sets this when WHO is asking decides the outcome — an authz
	// bundle whose point is that one requester has standing and another does
	// not cannot express itself otherwise.
	DefaultUser string `json:"defaultUser,omitempty"`

	// UserTurns are sent in order, each awaiting the agent's reply. Each entry
	// is either a bare string (the message text) or an object that also
	// carries attachments — see UserTurn.
	//
	// Mutually exclusive with Trigger: a run is started either by a person
	// typing on a conversational channel or by a provider delivering an event,
	// never both.
	UserTurns []UserTurn `json:"userTurns,omitempty"`

	// Trigger starts the run from a signed webhook delivery instead of a typed
	// message, so a scenario can cover the sessions nobody opens by hand.
	//
	// This is the only way a bundle reaches the behaviors gated on the INPUT
	// binding's kind — the trigger-status surface, the
	// trigger-status-concluded completion requirement, the session-opening line
	// that roots the thread. All three are inert on a `fake` input channel, and
	// giving `fake` a status surface it does not have would make the scenario
	// prove the fixture rather than the code.
	Trigger *Trigger `json:"trigger,omitempty"`

	// AutoApprove names approval categories a background watcher clears as they
	// appear, so a scenario can exercise flows that legitimately pause for a
	// human without the bundle having to interleave decisions into the
	// transcript. Without this a phase awaiting approval simply blocks until the
	// class timeout, and the run reads as a hang rather than as the pause it is.
	AutoApprove []string `json:"autoApprove,omitempty"`

	// FakeDeliverySurfaces turns on the `fake` kind's artifact-delivery
	// surfaces for this bundle's run: the asset capability respond_to_user
	// gates its `attached` field on, and a live_view_offer sender that records
	// what it is handed.
	//
	// Off by default, and deliberately so. A channel kind's capabilities decide
	// which meta tools a class is offered at all, so turning these on globally
	// would change the tool list under every bundle in the suite. A scenario
	// that wants to exercise how an artifact REACHES someone opts in here; the
	// driver restores the default when it finishes.
	//
	// Only affects fake-backed channels. A bundle whose output is a real kind —
	// the reviewbot pair is github in, slack out — already has both surfaces and
	// needs nothing here.
	FakeDeliverySurfaces bool `json:"fakeDeliverySurfaces,omitempty"`

	// AutoDeny is AutoApprove's negative: categories the watcher REFUSES as they
	// appear.
	//
	// A scenario that means "this stays outside the ceiling" has to answer the
	// request, not ignore it. Leaving a prompt unanswered stalls the session
	// until the class timeout, which reads as a hang rather than as the refusal
	// the scenario intends — and a bundle that hangs proves nothing about the
	// path it was written to cover.
	AutoDeny []string `json:"autoDeny,omitempty"`

	// AutoApproveAs is the identity the background approver decides as. Empty
	// means DefaultUser.
	//
	// For a tool approval the approver is normally NOT the requester — it is
	// whoever holds standing on the resource — so a multi-user scenario has to
	// be able to say who clicked. An identity without standing is refused by
	// the approval path, which is itself worth a scenario.
	AutoApproveAs string `json:"autoApproveAs,omitempty"`

	// MintedIDs are the ids the SYSTEM minted during the run, keyed by family
	// and in mint order. The replay hands them back from the same components
	// that minted them, so the tool arguments recorded below keep their literal
	// values. See mintedids.go — the sequence is an assertion about how many
	// ids the run mints and in what order, not just plumbing.
	//
	// A family absent here is unpinned: that component mints its own ids
	// exactly as in production, which is what a bundle naming no operation id
	// wants.
	MintedIDs map[string][]string `json:"mintedIDs,omitempty"`

	// ToolCatalogs is what the run was OFFERED, per turn, one entry per CHANGE.
	// The replay offers exactly this set at each turn and fails when it cannot.
	// See toolcatalogs.go — like MintedIDs, the recorded set is the assertion.
	//
	// Empty is unpinned and makes no claim: a bundle that records no catalog
	// runs with whatever tool set its fixture composes, exactly as before.
	ToolCatalogs []ToolCatalog `json:"toolCatalogs,omitempty"`

	// ExpectedExtraTools are tools the replay will have that the captured run
	// did not, and which must therefore NOT be reported as a regression.
	//
	// The only legitimate source is a gate the CAPTURE ITSELF removed to make
	// the bundle replayable — today, RewriteFixture dropping a SidecarToolbox's
	// spec.secretInputs, which in the live session held that sidecar's tools
	// back until a producer tool published the gating Secret. A fixture has no
	// producer, so the gate could never open; dropping it is what lets those
	// tools exist at all, and it makes them exist from the FIRST turn rather
	// than the turn they really arrived at.
	//
	// Every name here is DERIVED from what the rewrite did, never written by
	// hand: see steelthread.RewriteResult.UngatedTools. A transcribed list is
	// the failure mode this field would otherwise introduce — it would keep
	// excusing a tool long after the rewrite stopped ungating it.
	//
	// Naming a tool here does not make it offered. It is withheld like every
	// other extra; what it buys is silence instead of a failure.
	ExpectedExtraTools []string `json:"expectedExtraTools,omitempty"`

	// StandIn is the state a replay seeds into the stand-ins that take a real
	// provider's place. See standin.go for the rule that governs it.
	//
	// Empty is the ordinary case and pins nothing: a bundle whose run touched no
	// provider has nothing to stand in for.
	StandIn *StandIn `json:"standIn,omitempty"`

	// MetaToolReplies maps an LLM-facing META tool name to the reply the replay
	// serves in place of running it. The LAST resort of the three answers
	// standin.go ranks, and an admission of lost coverage rather than a feature
	// — see metatool.go, which owns every rule about it.
	//
	// Empty is the ordinary case: every meta tool runs for real, because a meta
	// tool IS the code under test. A bundle carrying an entry is refused unless
	// Assert.ToolsCalled names the same tool, and a capture that emits one
	// raises a warning naming what stopped running.
	MetaToolReplies map[string]MetaToolReply `json:"metaToolReplies,omitempty"`

	// LLM is the model's side, POSITIONAL: the nth Send gets the nth entry.
	LLM []LLMStep `json:"llm"`

	Assert Assertions `json:"assert"`
}

// Validate reports whether this bundle is STRUCTURALLY runnable — every
// precondition a driver needs satisfied before it boots a fixture.
//
// It lives on Bundle, in this package, because two consumers must give the same
// answer and must not drift: the replay driver refuses a bundle that fails it,
// and the steelthread capture's self-check raises a HARD finding for the same
// failure. Without one definition, a capture could report zero findings and
// write a bundle its own loader refuses — which is exactly what happened for a
// triggered session that a person also typed into, leaving UserTurns non-empty
// AND Trigger set. Same reason MintedIDFields lives here rather than in the
// driver.
//
// SHAPE only, never claims. A bundle whose assertions are weak, or whose
// evidence is thin, is a judgement for a human reading it — not a structural
// error, and not something a capture can decide.
//
// The first failure is returned rather than every one, matching
// ValidateToolOutputs and the fail-fast the driver already had: each message
// names one precondition, so the reader is never left guessing which.
func (b Bundle) Validate() error {
	if b.Name == "" {
		return fmt.Errorf("bundle needs a name")
	}
	if b.AgentDir == "" {
		return fmt.Errorf("bundle needs an agentDir")
	}
	if b.AgentClass == "" {
		return fmt.Errorf("bundle needs an agentClass")
	}
	if len(b.LLM) == 0 {
		return fmt.Errorf("bundle needs at least one llm step")
	}

	// A run starts one way or the other, never both and never neither. A bundle
	// with neither would otherwise boot a whole fixture, make no model call at
	// all, and fail on whatever assertion happened to be first — a diagnosis
	// several minutes away from the cause. A bundle with BOTH is ambiguous
	// about what opened the session, which is the one fact the trigger path
	// exists to exercise.
	if len(b.UserTurns) > 0 && b.Trigger != nil {
		return fmt.Errorf("a bundle starts from userTurns OR from a trigger, not both")
	}
	if len(b.UserTurns) == 0 && b.Trigger == nil {
		return fmt.Errorf("a bundle needs either userTurns or a trigger; nothing would start the run")
	}
	if b.Trigger != nil {
		if b.Trigger.Channel == "" {
			return fmt.Errorf("trigger needs a channel")
		}
		if b.Trigger.Payload == "" {
			return fmt.Errorf("trigger needs a payload")
		}
		if b.Trigger.Event == "" {
			return fmt.Errorf("trigger needs an event")
		}
		if b.Trigger.ChannelKey == "" {
			return fmt.Errorf("trigger needs a channelKey to wait on")
		}
	}

	if err := b.validateMintedIDs(); err != nil {
		return err
	}

	if err := b.validateToolCatalogs(); err != nil {
		return err
	}

	if err := b.validateStandIn(); err != nil {
		return err
	}

	if err := b.validateMetaToolReplies(); err != nil {
		return err
	}

	if err := b.validateSeeds(); err != nil {
		return err
	}

	return b.ValidateToolOutputs()
}

// validateSeeds refuses a seed the driver could not act on. Every field here
// is REQUIRED, unlike most of this format's optional fields, because a seed
// with a blank field is not "unpinned" the way an absent one is — it is a
// bundle claiming to set up fixture state and silently doing nothing (an
// empty Subject resolves no memory.UserScope; an empty Resource/Relation/
// Subject writes no relationship), which is a broken bundle, not a weaker
// one.
func (b Bundle) validateSeeds() error {
	for i, s := range b.SeedUserPreferences {
		if s.Subject == "" {
			return fmt.Errorf("seedUserPreferences[%d] has no subject", i)
		}
		if s.Key == "" {
			return fmt.Errorf("seedUserPreferences[%d] has no key", i)
		}
		if len(s.Value) == 0 {
			return fmt.Errorf("seedUserPreferences[%d] has no value", i)
		}
	}
	for i, r := range b.SeedRelationships {
		if r.Resource == "" {
			return fmt.Errorf("seedRelationships[%d] has no resource", i)
		}
		if r.Relation == "" {
			return fmt.Errorf("seedRelationships[%d] has no relation", i)
		}
		if r.Subject == "" {
			return fmt.Errorf("seedRelationships[%d] has no subject", i)
		}
	}
	return nil
}

// validateStandIn refuses seed state the replay could not act on.
//
// Every failure here is SILENT at replay if it is not caught, which is why it
// is a load precondition rather than a runtime check. A stand-in that was
// handed nothing usable does not error; it falls back to behaving as it does
// with no seed at all — minting its own ids, advertising no directory — and the
// bundle then diverges several steps later on something that reads as a
// product bug.
func (b Bundle) validateStandIn() error {
	if b.StandIn == nil {
		return nil
	}
	for i, id := range b.StandIn.TriggerStatusIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("standIn.triggerStatusIDs[%d] is empty; a stand-in cannot mint a blank "+
				"identifier, and a blank entry shifts every id after it by a position", i)
		}
	}
	if len(b.StandIn.TriggerStatusIDs) > 0 && b.Trigger == nil {
		return fmt.Errorf("standIn.triggerStatusIDs names %d provider-minted id(s) but the bundle has no "+
			"trigger; nothing would seed them and the claim would be silently unpinned",
			len(b.StandIn.TriggerStatusIDs))
	}

	m := b.StandIn.Mentions
	if m == nil {
		return nil
	}
	if len(m.Lookups) == 0 && len(m.Users) > 0 {
		return fmt.Errorf("standIn.mentions declares %d user(s) and advertises no lookup kinds; every "+
			"lookup would be refused as unsupported before the directory was consulted", len(m.Users))
	}
	for i, u := range m.Users {
		switch {
		case u.Kind == "":
			return fmt.Errorf("standIn.mentions.users[%d] has no kind", i)
		case u.Value == "":
			return fmt.Errorf("standIn.mentions.users[%d] has no value to match on", i)
		case u.ExternalID == "":
			return fmt.Errorf("standIn.mentions.users[%d] has no externalID; resolving to an empty id is "+
				"what the entry exists to prevent", i)
		case !slices.Contains(m.Lookups, u.Kind):
			return fmt.Errorf("standIn.mentions.users[%d] is recorded under kind %q, which is not "+
				"advertised (%v); nothing could ever look it up", i, u.Kind, m.Lookups)
		}
	}
	return nil
}

// validateMintedIDs refuses a mintedIDs map the replay could not honour.
//
// Every failure here is silent at replay if it is not caught: Minter reads the
// map by family NAME, so a typo'd key leaves the real family unpinned and the
// run mints its own ids — a bundle that looks like it pins a sequence and
// pins nothing. An id of the wrong family, or one that is not the minted shape
// at all, would be handed back by a component that never mints that shape, and
// the tool it reaches complains about an id it was given.
func (b Bundle) validateMintedIDs() error {
	for family, ids := range b.MintedIDs {
		known := false
		for _, f := range MintedIDFamilies {
			if f.Name == family {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("mintedIDs[%q] is not a minted-id family; the families are %v",
				family, familyNames())
		}
		seen := map[string]bool{}
		for _, id := range ids {
			got, ok := FamilyOf(id)
			if !ok {
				return fmt.Errorf("mintedIDs[%q] entry %q does not match any family's minted shape "+
					"(see MintedIDFamilies): most are a family prefix plus 16 lowercase hex digits, "+
					"which is what a memory-kind mint site emits", family, id)
			}
			if got != family {
				return fmt.Errorf("mintedIDs[%q] entry %q is an %q id: the replay would hand it back from "+
					"the wrong component", family, id, got)
			}
			if seen[id] {
				return fmt.Errorf("mintedIDs[%q] entry %q is recorded twice: two mints sharing one id alias "+
					"each other, so a recorded argument naming the second addresses the first", family, id)
			}
			seen[id] = true
		}
	}
	return nil
}

// familyNames is the family list as it reads in an error.
func familyNames() []string {
	out := make([]string, 0, len(MintedIDFamilies))
	for _, f := range MintedIDFamilies {
		out = append(out, f.Name)
	}
	return out
}

// Trigger is one signed webhook delivery, replayed through the production
// receive path.
//
// The delivery is REAL down to the HMAC: the driver signs the payload with the
// input Channel's own credentials Secret and posts it to the production
// webhook route, so a scenario cannot pass with signature verification, event
// filtering or channel-key derivation broken. Only the provider's API is a
// fixture, and only because a test cannot reach github.com.
//
// The Channel's OWN spec.kind selects the delivery mechanics; the bundle does
// not restate it. A bundle that named a kind could disagree with the fixture it
// ships, and the fixture is what the pipeline actually reads.
type Trigger struct {
	// Channel is the metadata.name of the fixture's INPUT Channel CR. It must
	// be in the harness namespace, and its kind must be one the driver knows
	// how to sign for (github today).
	Channel string `json:"channel"`

	// Payload names the JSON event body beside the bundle, at
	// <bundleDir>/payloads/<Payload>. A file rather than inline JSON, for the
	// reason ExtraManifests gives: a provider event escaped into a JSON string
	// is unreadable and unreviewable.
	Payload string `json:"payload"`

	// Event is the provider's event-type header value (github's
	// X-GitHub-Event). Named explicitly because the receiver dispatches on it
	// and a payload alone does not carry it.
	Event string `json:"event"`

	// ChannelKey is the binding key this delivery must produce — the driver
	// waits for exactly one AgentSession carrying it.
	//
	// Stated rather than derived: the key is what the trigger-status surface
	// parses back out to address the provider, so a bundle that recomputed it
	// the way the code does could not catch the format changing under it. It is
	// the same reasoning the git fixture's hand-written object id rests on.
	ChannelKey string `json:"channelKey"`

	// HeadSHA is the commit the fixture provider reports as the pull request's
	// current head.
	//
	// Deliberately NOT the sha in the payload: the status surface resolves the
	// commit it answers for by READING the pull request, so a value that could
	// only have come from that read is what proves the agent supplied none of
	// it.
	HeadSHA string `json:"headSHA,omitempty"`

	// StatusName is the name the provider files the status under — github's
	// check-run name, which the kind takes from the Channel's app slug.
	StatusName string `json:"statusName,omitempty"`
}

// TriggerStatusAssertions are claims about the status the run left on the
// TRIGGERING resource — for github, the check run on the pull request.
//
// Read from the fixture provider rather than from the tool result, for the
// reason every other assertion here is: `conclude_trigger_status` reports
// success to the model whether or not anything was written, and a scripted
// transcript would say the same words either way. The provider's own record is
// the only place "the pull request was answered" is distinguishable from "the
// agent believed it was".
type TriggerStatusAssertions struct {
	// Status and Conclusion are the provider's own vocabulary, not the
	// framework's — the mapping from `clean`/`problems_found` happens in the
	// kind, and asserting on the framework side would pass with that mapping
	// broken.
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`

	// ExternalID is the commit the status answers for.
	ExternalID string `json:"externalID,omitempty"`

	// SummaryContains asserts each string appears in the published summary —
	// the half a human reads on the pull request.
	SummaryContains []string `json:"summaryContains,omitempty"`

	// Creates and Updates count the provider calls: one create for the claim, one
	// update for the conclusion. The PAIR is the assertion that matters. A
	// check left `in_progress` is the defect this whole surface exists for, and
	// it shows up here as creates=1/updates=0; a conclusion that opened its own
	// run instead of patching the claimed one shows up as creates=2. Neither is
	// visible in the final status alone.
	Creates int `json:"creates,omitempty"`
	Updates int `json:"updates,omitempty"`
}

// ChannelAssertions are claims about what actually reached the bound OUTPUT
// channel — the messages a person sees.
//
// Distinct from AgentReplyContains, which reads only respond_to_user envelopes
// on a conversational channel. A triggered session's output surface carries
// more than replies: the session-opening line that names the trigger, and
// whatever else the run posts. Asserting on the surface's own record is the
// only way to claim a person was told, in the order they were told it.
type ChannelAssertions struct {
	// PostCount is the exact number of posts the output channel received.
	// Exact rather than "at least", because the failures worth catching here
	// are extra posts (a duplicate review, a re-posted opener) as much as
	// missing ones.
	PostCount int `json:"postCount,omitempty"`

	// PostsContain asserts each string appears in some post.
	PostsContain []string `json:"postsContain,omitempty"`

	// ThreadRootedByFirstPost asserts every later post is threaded under the
	// first one, and that the first is itself a thread root.
	//
	// The session-opening line is posted before any agent output, precisely so
	// the thread exists to hang the rest of the round under. A review that
	// landed top-level instead reads to a human as an unrelated message in a
	// busy channel, and nothing in the review's own text would show it.
	ThreadRootedByFirstPost bool `json:"threadRootedByFirstPost,omitempty"`
}

// LLMStep is one modelled turn of the conversation.
type LLMStep struct {
	// AgentClass names the class whose SESSION this step answers, scoping the
	// step to that session's own positional stream.
	//
	// A scenario with one session leaves it empty and the whole LLM list is one
	// shared stream, which is what every single-session bundle is. A scenario
	// where two sessions converse — a conversational delegation, where the
	// parent and the delegated child each take turns of their own — cannot be
	// written that way honestly: an unscoped step is answered by whichever
	// session asks next, so the child's turns would be scripted as though they
	// were the parent's and a delegation that never conversed at all could
	// still consume the whole transcript in order.
	//
	// Scoping is by the class's own composed system prompt, which carries
	// spec.systemPrompt.inline verbatim, so the claim "this turn is the
	// child's" is checked against the request rather than assumed from
	// position. Steps for one class stay positional among themselves.
	AgentClass string `json:"agentClass,omitempty"`

	// Expect is the divergence check. It is verified against the actual request
	// BEFORE the reply is returned, so a system that took a different path
	// fails loudly instead of being handed an answer to a question it did not
	// ask.
	//
	// This is what makes positional replay safe, and it is the same field
	// steelthread derives from a capture.
	Expect Expect `json:"expect"`

	Reply []ReplyPart `json:"reply"`
}

// Expect describes the request an LLMStep is the answer to. Empty fields are
// not checked, so a bundle asserts as much or as little as is meaningful.
type Expect struct {
	// UserTextContains matches the most recent user text block.
	UserTextContains string `json:"userTextContains,omitempty"`

	// LastToolResult names the tool whose result should be the latest message.
	LastToolResult string `json:"lastToolResult,omitempty"`

	// LastToolResultContains asserts on the CONTENT of that result, and
	// LastToolResultIsError on whether it came back as an error.
	//
	// Without these a transcript cannot tell a tool that succeeded from one
	// that was refused: the scripted reply is identical either way, so a bundle
	// asserting only on the agent's words would stay green while every call was
	// denied. Any bundle whose point is an authorization outcome should pin the
	// result the model was actually handed.
	LastToolResultContains string `json:"lastToolResultContains,omitempty"`

	// LastToolResultNotContains asserts something is ABSENT from the result —
	// the direct way to state that data the caller was refused did not reach
	// the model anyway. A refusal that still leaks the payload into context has
	// denied nothing.
	LastToolResultNotContains string `json:"lastToolResultNotContains,omitempty"`

	// LastToolResultIsError is a pointer so a bundle can assert "must NOT be an
	// error" (false) distinctly from not caring (unset).
	LastToolResultIsError *bool `json:"lastToolResultIsError,omitempty"`

	// ToolResultCounts asserts the exact result population in the latest
	// tool-result message, grouped by tool name. It is the order-independent
	// form for one assistant reply that emitted multiple tool uses: those calls
	// dispatch concurrently, so completion/admission order is not a stable
	// property, while the number that succeeded or failed is.
	ToolResultCounts map[string]ToolResultCount `json:"toolResultCounts,omitempty"`

	// ToolOffered asserts a tool is in the request's catalog. Useful for
	// capability-gating scenarios (is select_phase offered when the gate is on?).
	ToolOffered string `json:"toolOffered,omitempty"`

	// ToolNotOffered is its negative.
	ToolNotOffered string `json:"toolNotOffered,omitempty"`

	// Capture pulls values out of the tool result this step is answering and
	// binds them to names that LATER steps' toolUse args interpolate as
	// {{name}}. Each value is a regular expression with exactly ONE capture
	// group, matched against the result's content.
	//
	// It exists for the values a static transcript cannot know because the
	// cluster mints them: a delegation handle is a GenerateName'd
	// SubagentRequest, so the parent's reply_to_subagent call has to name
	// something no bundle author can write down. A real model reads it out of
	// the result text it was just handed; this is the transcript doing exactly
	// that, and nothing more — the value comes from the run, never from the
	// bundle.
	//
	// A capture whose pattern does not match is a failure, not an empty
	// binding: substituting "" would send a well-formed call naming nothing
	// and turn a broken transcript into a plausible-looking refusal.
	Capture map[string]string `json:"capture,omitempty"`
}

// ToolResultCount is one tool's expected result summary in an LLM request.
type ToolResultCount struct {
	Total  int `json:"total"`
	Errors int `json:"errors"`
}

// ReplyPart is one element of the model's response. Exactly one field is set.
type ReplyPart struct {
	Text    string   `json:"text,omitempty"`
	ToolUse *ToolUse `json:"toolUse,omitempty"`
	EndTurn bool     `json:"endTurn,omitempty"`

	// BareText is a text block the model emitted WITHOUT calling
	// respond_to_user — narration alongside a tool call.
	//
	// Distinct from Text, which is shorthand for "reply by calling
	// respond_to_user" and emits a text block AND that call. An authored bundle
	// almost always means the shorthand; a CAPTURE routinely finds bare text,
	// because a real model narrates before it acts. Without this field the fold
	// would have to drop that narration, which quietly changes the message
	// history the replay reconstructs.
	BareText string `json:"bareText,omitempty"`
}

type ToolUse struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// ToolError is one upstream JSON-RPC error the fake MCP server returns.
type ToolError struct {
	// Code is the JSON-RPC error code. Optional, and DefaultToolErrorCode is
	// used when it is zero.
	//
	// A capture cannot observe it: the runner records the tool result as
	// "mcp: " plus the MCP client's rendering of the failure, and the numeric
	// code is nowhere in the transcript. It is in the format anyway because an
	// AUTHORED bundle covering a specific upstream failure mode may need to
	// name one, and because the stub's wire response has to carry something.
	Code int `json:"code,omitempty"`

	// Message is the JSON-RPC error message, and the half that actually
	// reaches the model: the client renders it into the tool result the
	// transcript then records.
	//
	// A capture sets it to the error text it OBSERVED, verbatim. That nests
	// the original text inside the replayed one rather than reproducing it
	// byte-for-byte — which is what a derived Expect needs, because
	// LastToolResultContains is a substring check.
	Message string `json:"message"`
}

// DefaultToolErrorCode is the code the driver registers when a ToolError names
// none. -32000 is the start of JSON-RPC 2.0's implementation-defined
// server-error range, which is what an application-level tool failure is.
const DefaultToolErrorCode = -32000

// Assertions are declarative claims about the finished run.
//
// Bronzethread asserts PROPERTIES rather than diffing a golden trace: a
// hand-authored golden would only restate the hand-authored input. Once a
// scenario is verified here, its emitted trace can be frozen as a golden and
// the same bundle becomes a steelthread-mode regression test.
type Assertions struct {
	// AgentReplyContains is checked against each user turn's reply, in order.
	AgentReplyContains []string `json:"agentReplyContains,omitempty"`

	// ApprovalPrompts, when set, is the EXACT number of interaction approval
	// prompts the run published, category-agnostic. Counting cards is what
	// distinguishes "the approval persisted" from "it was re-granted per
	// call" — and, for a gate that parks before any tool exists (the guest
	// session-start gate), it is the only prompt count available: the
	// planGate.approvalPrompts sibling requires plan-gate audit records a
	// pre-runner park never writes.
	ApprovalPrompts *int `json:"approvalPrompts,omitempty"`

	// AgentReplyNotContains is the negative companion, checked against the SAME
	// per-turn reply, in order: the reply to turn i must NOT contain
	// AgentReplyNotContains[i]. The only way a bundle can claim a person did not
	// see something in the reply — a provenance delimiter, a leaked nonce, an
	// envelope marker that should have been stripped. A scripted reply says
	// whatever the transcript wrote, so this is checked on the DELIVERED text,
	// not asserted from the transcript. An entry may be paired with the same
	// turn's AgentReplyContains (both predicates run on one reply) or stand alone
	// (leave the Contains slot ""); a turn with neither is not awaited.
	AgentReplyNotContains []string `json:"agentReplyNotContains,omitempty"`

	// SystemPromptContains asserts each string appears in the system prompt the
	// runner actually composed and sent.
	//
	// The only assertion here that reads the prompt rather than the run. A claim
	// about prompt COMPOSITION — that a class's authored plan examples reach its
	// agent, that a toolkit's guidance is rendered — cannot be made any other way
	// in a scripted bundle: the model is scripted, so its reply says whatever the
	// transcript says regardless of what it was told. Asserting on the reply
	// would pass with the prompt empty.
	SystemPromptContains []string `json:"systemPromptContains,omitempty"`

	// SystemPromptNotContains is SystemPromptContains' negative: each entry
	// must be ABSENT from the composed system prompt. It exists for the gate
	// a capability's prompt text is behind — a class with no AgentUI must get
	// no "Your page" section — because a presence assertion on the bundle that
	// has the page proves nothing about the bundle that must not.
	SystemPromptNotContains []string `json:"systemPromptNotContains,omitempty"`

	// NoticesPublished names interaction categories the run must have posted to
	// the bound channel.
	//
	// The way a bundle claims A PERSON WAS TOLD. Nothing else in a scripted run
	// can: the agent's reply is authored by the transcript and says whatever the
	// bundle wrote, and a tool result only proves what the MODEL was handed. A
	// system message about the agent — a bypass, a degradation, a failure — goes
	// out on the interaction wire instead, so the published set is the only
	// place its absence would show.
	NoticesPublished []string `json:"noticesPublished,omitempty"`

	// NoticesNotPublished names interaction categories the run must NOT have
	// posted. The negative half of NoticesPublished, and the only way a bundle
	// can claim a person was told THE RIGHT THING rather than merely told
	// something.
	//
	// It exists because the failure it guards is a WRONG notice, not a missing
	// one: an attachment problem that published the transient category for a
	// permanent cause satisfied every positive assertion available while
	// telling the user to retry something that could never succeed. A positive
	// set cannot express "and not that one".
	//
	// PAIR IT WITH NoticesPublished. The driver waits for the positive set
	// to arrive and then checks absence against what landed; asserting only
	// absence checks a channel nothing has reached yet and passes vacuously.
	NoticesNotPublished []string `json:"noticesNotPublished,omitempty"`

	// NoticeCounts asserts the EXACT number of cards published per interaction
	// category (keyed as NoticesPublished is: string(Payload.Category)).
	//
	// PlanGate.ApprovalPrompts is this same guard for the plan-gate category
	// alone — invented to catch a cleared phase re-asked per call because the
	// decision never reached the log. Nothing gave the other categories
	// (info_leakage, credential_update, session_join, …) that guard: their
	// membership-only NoticesPublished check is satisfied whether a card was
	// raised once or five times, so a notice duplicated per retry rode through.
	// This states the degree. A count higher than expected is a re-raised card; a
	// lower one, a notice that never reached the person.
	NoticeCounts map[string]int `json:"noticeCounts,omitempty"`

	// PlanGate asserts on the plan_gate_audit log.
	PlanGate *PlanGateAssertions `json:"planGate,omitempty"`

	// PtTags asserts on the per-datum provenance tags the run minted.
	PtTags *PtTagAssertions `json:"ptTags,omitempty"`

	// Authz asserts on the authz_decision log.
	Authz *AuthzAssertions `json:"authz,omitempty"`

	// SpiceDB asserts relationship-derived permissions directly against the
	// harness's real SpiceDB, fully-consistent — NOT through the authz_decision
	// log. Authz records only what a DISPATCHED tool call happened to Check; a
	// tuple written but never exercised by a call in the transcript is invisible
	// to it, and a grant that leaks across sessions is caught only if some later
	// call is gated on it. This states the relationship outcome outright: after
	// the run, does permission P hold (or not) for subject S on resource R.
	//
	// A resource id is named in its ON-THE-WIRE form — escaped / canonicalized,
	// the same spelling a SpiceDBBootstrap grant uses — because that is what a
	// Check resolves against. A `user:` SUBJECT is the one exception: name it by
	// raw email (`user:alice@example.com`), which the driver canonicalizes to the
	// escaped id SpiceDB stores (`@`/`.` are illegal in an object id), exactly as
	// the pt-tag reader assertions do. Any other subject type is on-the-wire too.
	SpiceDB []SpiceDBCheck `json:"spicedb,omitempty"`

	// TriggerStatus asserts on the status the run left on the triggering
	// resource. Only meaningful for a bundle with a Trigger.
	TriggerStatus *TriggerStatusAssertions `json:"triggerStatus,omitempty"`

	// Channel asserts on the posts the bound output channel received.
	Channel *ChannelAssertions `json:"channel,omitempty"`

	// ToolsCalled names tools the run must have DISPATCHED, read back off the
	// replayed session's own transcript.
	//
	// # Why this is derivable where toolOffered is not
	//
	// The steelthread capture declines to derive Expect.ToolOffered on purpose:
	// WHICH tool mattered to a scenario is a judgement, and a capture that
	// picked one would be inventing a claim nobody made. "This tool was called"
	// is not a judgement. It is a fact sitting in the transcript, so a capture
	// can state it without guessing, and a human reading the bundle can see at
	// once what the run actually drove.
	//
	// # What it adds over the positional Expect
	//
	// Expect.LastToolResult already pins, per step, which tool the model was
	// answering — a strong check, and the reason a diverging run fails early.
	// This is its non-positional companion, and it covers the case that one
	// cannot: a step whose Expect was NARROWED away. A tool result the capture
	// could not pin every byte of leaves LastToolResultContains empty, and a
	// call that stopped happening at all then shifts the transcript rather than
	// failing outright. Naming the call itself is what keeps a silently-skipped
	// tool from riding through.
	//
	// It is the MANDATORY companion for any reply this format cans in place of
	// running the real code, and mandatory STRUCTURALLY: Bundle.Validate
	// refuses a MetaToolReplies entry whose tool is not named here (see
	// validateMetaToolReplies), and the driver and the capture's self-check
	// both refuse on that same method. A canned reply is served whether or not
	// the call it stands for was made the way the recording made it, so without
	// a separate statement that the call happened, such a bundle proves nothing
	// about it.
	//
	// The rule predates the canning it governs. It was written when nothing
	// cans a meta tool's reply, so that whoever first wanted to could not do it
	// silently — and the refusal above is what closed the gap between the rule
	// and its enforcement in the same change that gave it a subject.
	//
	// Names are as the MODEL sees them, which is how the transcript records
	// them. Order is not asserted — that is Expect's job, positionally.
	ToolsCalled []string `json:"toolsCalled,omitempty"`
	// ArtifactsDelivered names the artifact IDs that must actually have
	// reached the bound output channel, in any order.
	//
	// It closes the one gap a delivery bundle cannot otherwise reach.
	// respond_to_user returning "delivered" proves the handle resolved, the
	// entitlement passed and an envelope was published — but NOT that the
	// render travelled with it. Those come apart: an attachment list built
	// empty, or dropped between the tool and the transport, satisfies every
	// text assertion in this file while the person sees a bare message where a
	// report should be.
	//
	// Asserted against the transport's OWN record (the fake kind's Sent()
	// payloads), for the same reason NoticesPublished reads the notice log
	// rather than the transcript: a scripted reply says whatever the bundle
	// told it to.
	//
	// Matched as a SUBSTRING of AttachmentRef.ArtifactID, the same idiom as
	// PostsContain — and necessary here, because an artifact ID is minted at
	// run time and a bundle cannot name it in advance. Capture interpolation
	// is not available: {{name}} substitution runs on tool ARGS only, and the
	// capture map is deliberately lock-free because nothing outside the LLM's
	// own mutex touches it, so reading it from the assertion goroutine would
	// be a data race rather than a missing feature.
	//
	// "artifact-" is therefore the useful entry, and it is not weak when the
	// bundle arranges for exactly one artifact to exist: a parent granted no
	// artifact capability of its own can only deliver a render some child
	// returned. State that argument in the bundle's description when relying
	// on it.
	//
	// Note which identifier this is. An agent sees a HANDLE ("ar-…", what
	// artifact_prepare returns and what a bundle captures); the artifact ID on
	// the wire is a different string ("artifact-…", the render's artifact-id
	// label). Asserting the handle form here matches nothing and fails with a
	// message naming what did arrive.
	ArtifactsDelivered []string `json:"artifactsDelivered,omitempty"`
}

// SeedUserPreference is one saved preference value written into a user's own
// memory scope before the run — see Bundle.SeedUserPreferences.
type SeedUserPreference struct {
	// Subject names the user the value is saved for: a bare canonical id, or
	// "email:<address>" — canonicalized the driver the same way
	// subjectresolve's own email resolver does (identity.EmailReference(...)
	// .Canonical()), so a bundle can spell the same person here and in a
	// SeedRelationship's "user:<email>" subject and land on the identical
	// canonical id without computing it by hand.
	Subject string `json:"subject"`
	// Key is the preference key; must match a name in the fixture class's
	// spec.userPreferences for the value to ever be resolved.
	Key string `json:"key"`
	// Value is the saved value, matching the key's declared type — the same
	// shape set_preference's own `value` argument takes.
	Value json.RawMessage `json:"value"`
}

// SeedRelationship is one SpiceDB relationship written before the run
// starts — see Bundle.SeedRelationships.
type SeedRelationship struct {
	// Resource is "<type>:<id>", on-the-wire form — the same convention
	// SpiceDBCheck.Resource documents.
	Resource string `json:"resource"`
	// Relation is the relation to write (e.g. "sole_user").
	//
	// Not every relation is writable by the driver's generic guarded writer:
	// spicedb.TypedWritesSource CLAIMS four relations (github_user#sole_user
	// among them) precisely so no relsource-bound writer may touch them, and
	// the only legitimate writer is *spicedb.Client's own typed helper. The
	// driver special-cases github_user#sole_user onto TouchSoleIdentity
	// today; a bundle needing another claimed relation (github_user#user,
	// agentsession#parent/#child) would need the same routing added to the
	// driver first. A bundle author names the relation the same way
	// regardless — the routing is the driver's problem, not this field's.
	Relation string `json:"relation"`
	// Subject is "<type>:<id>", on-the-wire form, with the SAME exception
	// SpiceDBCheck.Subject documents: a "user:" subject is named by RAW
	// EMAIL and canonicalized by the driver, because nothing else in an
	// authored bundle can spell a canonical id by hand. Any other subject
	// type is passed through verbatim.
	Subject string `json:"subject"`
}

// SpiceDBCheck is one relationship-derived permission assertion: after the run,
// CheckPermission(Resource, Permission, Subject) must equal Want.
type SpiceDBCheck struct {
	// Resource is "<type>:<id>", the id in its on-the-wire (escaped/canonical)
	// form (e.g. "git_repo:https=3A//github=2Ecom/demo-org/demo-repo").
	Resource string `json:"resource"`
	// Permission is the relation/permission name to check (e.g. "read").
	Permission string `json:"permission"`
	// Subject is "<type>:<id>" (e.g. "user:alice@example.com").
	Subject string `json:"subject"`
	// Want is the expected permissionship: true = HAS_PERMISSION.
	Want bool `json:"want"`
}

// AuthzAssertions are claims about what the tool dispatcher's Check actually
// decided, read from the authz_decision log.
//
// The log rather than the transcript, because a scripted model reports whatever
// the bundle told it to: a transcript can say "access denied" while the call
// sailed through. Only the decision record distinguishes those.
//
// Keys are "<resourceType>:<resourceID>#<permission>" — the same triple the
// slot grant is keyed on, so a bundle states the instance-and-permission pair
// it means without a second naming scheme.
type AuthzAssertions struct {
	// Decisions maps a key to the outcome finally recorded for it ("allowed" |
	// "denied"). Final, not first: a call that is denied, approved, and then
	// re-dispatched records both, and the resolved outcome is the one that
	// decided whether the tool ran.
	Decisions map[string]string `json:"decisions,omitempty"`

	// NeverAllowed lists keys that must have NO allowed decision at any point.
	// This is the airtight form for an isolation claim — "denied then allowed"
	// and "never allowed" have the same final outcome only by accident, and a
	// permission boundary is worth asserting the strong way.
	NeverAllowed []string `json:"neverAllowed,omitempty"`
}

// PtTagAssertions are claims about the per-datum provenance tags a run minted.
//
// # Why a bundle can claim anything here at all
//
// A scripted transcript says the same words whether a read was tagged or not,
// so nothing in the reply or the tool result distinguishes a working lattice
// from an absent one. The tags are the artifact the mint actually produced,
// and their reader set is DERIVED server-side from the resource the call
// touched — never named by the session. That makes them the one place a
// scenario can assert on confidentiality rather than on narration.
type PtTagAssertions struct {
	// Minted is how many tags the run must have minted. Zero means "do not
	// check"; use MintedNone to assert none.
	//
	// The count rather than "some", for the reason the plan gate's
	// ApprovalPrompts is a count: a hook that mints per RETRY rather than per
	// read satisfies "at least one" while quietly multiplying a session's
	// provenance records, and a boolean cannot tell those apart.
	Minted int `json:"minted,omitempty"`

	// MintedNone asserts the run minted nothing — the shape of a scenario
	// where per-datum provenance is off, or where every tool call was
	// unmapped.
	MintedNone bool `json:"mintedNone,omitempty"`

	// ReadersInclude names USER EMAILS that must appear in the resolved reader
	// set of EVERY minted tag. The driver canonicalizes each before comparing,
	// the same affordance SpiceDBBootstrap's `canonicalize: true` offers —
	// SpiceDB stores a canonical subject ID (base64url of the email), and a
	// bundle carrying those blobs literally would be unreadable and would
	// silently rot the day the encoding moved.
	//
	// Resolved through SpiceDB, not read off the record's directReaders: the
	// record is the durable account, the permission is the enforcement input,
	// and a bundle that checked the record alone would pass on a tag whose
	// tuples were never written. `reader` is also an INTERSECTION across
	// derived_from, so only the resolved answer reflects what a chain of
	// derivations actually narrowed to.
	ReadersInclude []string `json:"readersInclude,omitempty"`

	// ReadersExclude names user emails that must NOT be readers of any minted
	// tag. The half that makes ReadersInclude worth asserting: a lattice that
	// resolved every subject to a reader would satisfy the positive set alone.
	ReadersExclude []string `json:"readersExclude,omitempty"`

	// CarriesUntrusted, when set, asserts EVERY minted tag's integrity answer
	// matches. Pointer so a bundle can assert FALSE — the default-valued
	// field would otherwise be indistinguishable from "do not check", and
	// "this data is NOT untrusted" is exactly the claim a trifecta scenario
	// needs to pin before it can blame leg A for a refusal.
	CarriesUntrusted *bool `json:"carriesUntrusted,omitempty"`

	// UntrustedCount asserts exactly how many of the minted tags carry an
	// untrusted origin. Pointer, to distinguish "expect none" from "unset".
	//
	// This is the form that proves the integrity axis is READ rather than
	// constant. A run whose reads are all trusted, or all untrusted, passes
	// CarriesUntrusted whether the source declaration is consulted or hard-
	// coded; a run that mints both and pins the split cannot. Pair it with
	// two otherwise-identical tools differing only in their declaration, and
	// the assertion is a controlled comparison rather than a claim.
	UntrustedCount *int `json:"untrustedCount,omitempty"`
}

// PlanGateAssertions are claims about what the gate recorded.
type PlanGateAssertions struct {
	// FrozenPhases is how many phases the agent's declaration should have
	// frozen. Zero means "do not check".
	FrozenPhases int `json:"frozenPhases,omitempty"`

	// OnePlanDigest asserts every frozen phase belongs to ONE plan.
	OnePlanDigest bool `json:"onePlanDigest,omitempty"`

	// RebuildableFromLog asserts the frozen plan reconstructs from the records
	// with an identical digest — the invariant whose violation made a forked
	// child silently inherit nothing.
	RebuildableFromLog bool `json:"rebuildableFromLog,omitempty"`

	// GatedAgainstFrozenPlan asserts permissioned calls were gated against the
	// frozen plan rather than a stale snapshot — the invariant whose violation
	// made the whole ceiling-narrowing feature inert.
	GatedAgainstFrozenPlan bool `json:"gatedAgainstFrozenPlan,omitempty"`

	// Outcomes maps an LLM-facing tool name to the outcome its call must have
	// recorded ("allow" | "would_deny"). This is how a scenario pins that a
	// ceiling actually narrowed.
	Outcomes map[string]string `json:"outcomes,omitempty"`

	// NoRecords asserts the gate wrote nothing (a disabled-mode scenario).
	NoRecords bool `json:"noRecords,omitempty"`

	// PublishedNothing asserts no approval reached a channel, which is
	// logging mode's defining property.
	PublishedNothing bool `json:"publishedNothing,omitempty"`

	// ApprovalPrompts is how many approval cards the gate must have PUBLISHED.
	//
	// The count, not merely "some", because the failure this pins is one of
	// degree: a phase whose approval is not written back to the log is folded
	// as unapproved on every subsequent call, so it re-asks per call and the
	// scenario still "works" — a human just gets N identical cards. A boolean
	// cannot tell those apart. Zero means "do not check"; use PublishedNothing
	// to assert none.
	ApprovalPrompts int `json:"approvalPrompts,omitempty"`

	// CardWhatContains asserts each string appears in the "What" field of some
	// published approval card.
	//
	// The What, specifically, and not the prompt as a whole: it is the half
	// computed from the frozen plan, and asserting against the whole payload
	// would pass on a string that appeared only in the agent's own Why — which
	// is the confusion the trust split exists to prevent.
	CardWhatContains []string `json:"cardWhatContains,omitempty"`

	// CardWhatOmits asserts none of these appear in any published card's What.
	//
	// The negative is the load-bearing direction here. Over-promising is
	// invisible to a contains-only assertion: a card that names a concrete
	// resource the agent never declared, or that lets agent-authored text into
	// the computed half, satisfies every positive claim you can write.
	CardWhatOmits []string `json:"cardWhatOmits,omitempty"`
}

// Capture is the provenance of a steelthread bundle.
type Capture struct {
	// Session is the namespace/name the transcript was taken from.
	Session string `json:"session"`
	// Cluster is the kubecontext at capture time — which cluster's session
	// this was, for a reader trying to find it again.
	Cluster string `json:"cluster,omitempty"`
	// CapturedAt is when the capture ran, NOT when the session ran.
	CapturedAt time.Time `json:"capturedAt"`
	// ServedModel is the uniform display id of the model that actually served
	// the turns ("<provider>/<served-model>").
	//
	// The field that makes a stale capture greppable. When a model is retired,
	// the bundles whose evidence rests on it can be listed and re-captured —
	// without it, a suite quietly keeps claiming a model produced something
	// years after that model stopped existing.
	ServedModel string `json:"servedModel,omitempty"`
	// OapVersion is the oap build that produced the bundle.
	OapVersion string `json:"oapVersion,omitempty"`

	// Redactions records the explicit replacements the capture applied to the
	// emitted bytes: one entry per supplied rule, naming the token it
	// substituted IN and how many times it fired.
	//
	// The ORIGINAL value is deliberately absent, and that absence is the whole
	// point. A bundle recording "acme-corp became COMPANY-A" would carry the
	// very string the operator asked to have removed into the same repo the
	// redaction existed to keep it out of, so the record would defeat the
	// mechanism it documents.
	//
	// What is left is still what a reader needs: the tokens say which strings
	// in the transcript are stand-ins rather than things the session really
	// said, and the counts say how much was rewritten. A rule that matched
	// NOTHING is recorded with a zero count rather than dropped — "this rule
	// did not fire" is exactly the fact a reader chasing a leftover identifier
	// needs, and a typo'd rule is indistinguishable from a rule with nothing to
	// do once it is dropped.
	Redactions []Redaction `json:"redactions,omitempty"`

	// SkillElisions records the operator-declared skill-source substitutions
	// the capture applied: a third-party repo authority replaced everywhere it
	// is referenced, and that source's skill CONTENT dropped.
	//
	// Recorded for the reason Redactions is, and with the same asymmetry — the
	// ORIGINAL authority is deliberately absent, because a bundle naming the
	// org it exists to keep out of the repo would defeat itself. What is here
	// is what a reader needs: which authority in the emitted manifests is a
	// stand-in rather than a real repo, and how much was removed under it.
	//
	// A reader seeing an entry here knows the emitted Skill bodies are NOT what
	// the session ran against. That is only ever safe for a skill the class
	// targets at the sandbox, whose content reaches no system prompt; the
	// capture refuses to elide any other, so an entry here is also the record
	// that the refusal did not fire.
	SkillElisions []SkillElision `json:"skillElisions,omitempty"`
}

// SkillElision is one operator-declared skill-source substitution a capture
// applied, recorded by its replacement authority and its counts. See
// Capture.SkillElisions for why the original authority is not here.
type SkillElision struct {
	// Authority is the repo locator substituted IN — the stand-in a reader
	// will find in the emitted SkillSource, Skill and AgentClass manifests
	// where the real one stood.
	Authority string `json:"authority"`
	// Sources is how many SkillSource CRs were rewritten onto it.
	Sources int `json:"sources"`
	// Skills is how many Skill CRs were rewritten onto it and emptied.
	Skills int `json:"skills"`
	// ElidedBytes is how much skill content — bodies, descriptions, repo
	// instructions, the non-identity frontmatter — was dropped.
	ElidedBytes int `json:"elidedBytes"`
}

// Redaction is one explicit replacement a capture applied, recorded by its
// replacement token and hit count. See Capture.Redactions for why the original
// value is not here.
type Redaction struct {
	// Replacement is the token substituted IN — the string a reader will
	// actually find in the bundle where the original stood.
	Replacement string `json:"replacement"`
	// Count is how many occurrences were replaced across every emitted file.
	// Zero means the rule was supplied and matched nothing.
	Count int `json:"count"`
}
