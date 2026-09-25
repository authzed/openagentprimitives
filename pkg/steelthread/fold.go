package steelthread

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
)

// respondToUserToolName is the meta tool a reply to a person goes out through.
// bt.ReplyPart.Text is shorthand for exactly this call, which is why the fold
// has to recognize it by name rather than treating it as one tool among many.
const respondToUserToolName = "respond_to_user"

// FoldOptions carries what the transcript alone cannot say.
type FoldOptions struct {
	// MCPPrefixes are the AgentClass's declared mcpServers[].name values.
	//
	// Read from the class rather than guessed from the tool name: the runner
	// synthesizes an LLM-facing name as "<prefix>_<tool>", and an underscore in
	// a meta tool's name would make a guess wrong in the direction that
	// silently produces a toolOutputs entry for a tool that runs for real.
	MCPPrefixes []string

	// SandboxPrefixes are the class's declared toolBundles[].name values — the
	// LLM-facing prefix a sandbox tool's name carries, exactly as MCPPrefixes is
	// for an MCP server.
	//
	// Separate from MCPPrefixes although both split a name the same way, because
	// what happens AFTER the split differs: an MCP result is a JSON-RPC result
	// the fake server hands back verbatim, while a sandbox result is composed
	// from process streams the fake exec binder answers with, and the fold has
	// to invert that composition to recover what the process printed.
	SandboxPrefixes []string
}

// Folded is the transcript half of a bundle.
type Folded struct {
	UserTurns          []string
	LLM                []bt.LLMStep
	ToolOutputs        map[string]json.RawMessage
	ToolOutputSequence map[string][]json.RawMessage

	// ToolErrors are the UPSTREAM failures, keyed SERVER-side — the bundle's
	// bt.Bundle.ToolErrors, ready to hand to the driver's OnToolError.
	//
	// Upstream only. A call the platform REFUSED never reached the server (see
	// gateRefused), so it contributes nothing here and nothing to ToolOutputs
	// either: the replay's own gate regenerates the refusal from the fixture.
	ToolErrors map[string]bt.ToolError

	// ErrorResults are the MCP tool calls whose recorded result was an UPSTREAM
	// error, in transcript order.
	//
	// Kept off ToolOutputs entirely: an error body is a plain string, so
	// storing it as the json.RawMessage that field holds produced a bundle
	// that could not even be marshalled, and the capture died at write time
	// with `invalid character 'a' looking for beginning of value` naming no
	// tool, no turn and no reason. They live here so the uniformity check below
	// and the self-check's report can both name the tool, the call and the
	// turn.
	ErrorResults []ErrorResult

	// MetaToolReplies are the CANNABLE meta tools' replies, keyed by LLM-facing
	// name — the bundle's bt.Bundle.MetaToolReplies, ready for the driver's
	// canner.
	//
	// A meta tool contributes nothing here unless bt.MetaToolCannable says its
	// answer comes from state a replay cannot reproduce. Every other meta tool
	// IS the code under test and runs for real; canning one would hand a
	// regression in it the recorded answer.
	//
	// SUCCESSES only. An error result is never canned — see the fold's meta
	// branch for why both kinds of error say so — and a tool that both errored
	// and succeeded is named in NonUniformMetaTools instead.
	MetaToolReplies map[string]bt.MetaToolReply

	// NonUniformMetaTools are the cannable meta tools whose calls did not all
	// answer the same way — two successes with different bodies, or a success
	// and an error. Sorted.
	//
	// The exact shape NonUniformErrorTools is, one layer over: bt.MetaToolReply
	// is one body per tool NAME, served on every call, so a tool that varied
	// has no spelling in the format at all. Named rather than dropped, because
	// canning the first body would serve it to a call that recorded something
	// else and the step's own expectation would fail somewhere downstream.
	NonUniformMetaTools []string

	// NonUniformErrorTools are the SERVER-side tool names whose calls did not
	// all return the SAME upstream error — one that errored and also succeeded,
	// or that errored twice with different text. Sorted.
	//
	// The single thing toolErrors still cannot express, and it is a property of
	// the registration rather than of the format: MCPStub.OnToolError is keyed
	// by tool NAME, wins over OnTool unconditionally, and is not counted, so
	// there is no per-call variation to reach for. Both shapes are one entry
	// here because they are one mechanism.
	NonUniformErrorTools []string

	// SecretOutputTools are the LLM-facing sandbox tool names whose recorded
	// result was a SECRET-OUTPUT DIVERSION rather than process output, sorted.
	//
	// The runner replaces such a result wholesale with
	// "<description>\n<secret-output name=… ref=… bytes=N>" before anything
	// records it, so what was recorded is a description line and an opaque
	// handle. The fold takes the description as the tool's stdout — for a
	// `file:`-sourced output that IS the producer's real stdout — but a reader
	// must not have to work that out from the bundle alone, so the self-check
	// says so.
	SecretOutputTools []string

	// StreamResultTools are the LLM-facing sandbox tool names whose recorded
	// result was a STREAMING or INTERACTIVE toolkit's composed text, carried
	// verbatim and served back rather than re-composed, sorted.
	//
	// A bundle carrying one replays a recording of the toolkit's result instead
	// of exercising the parse that produced it, and nothing in the emitted file
	// makes that obvious to a reader scanning for it — so the self-check says
	// which tool, in a warning. Reported by the fold for the same reason
	// SecretOutputTools is: this is the only place that saw the raw result text.
	StreamResultTools []string

	// UnreplayableSandboxTools are the LLM-facing sandbox tool names whose
	// recorded result matched NO composition the fold can invert, sorted by
	// name, each carrying which KIND of result defeated it.
	//
	// The kind is carried because the two have nothing in common but the
	// refusal. A failed call whose terminal condition is not a plain non-zero
	// exit is one thing; a SUCCESSFUL call whose result was composed by a
	// streaming or interactive toolkit is another, and the finding that says
	// "a failed call is the usual cause" about a call that succeeded sends the
	// reader looking for a failure that is not there.
	UnreplayableSandboxTools []SandboxRefusal

	// ServedModel is the model that actually served the turns, for Capture.
	ServedModel string

	// MintedIDs are the ids the RUN minted, keyed by bt family and in mint
	// order — the bundle's mintedIDs, ready for the replay's own minters.
	//
	// Recorded rather than rewritten. An earlier scheme replaced each minted id
	// with a `$` sentinel the driver substituted back, and it could not express
	// the ordinary case: update_plan mints one operation per item it moves to
	// in_progress and returns them nested inside its item array, so one
	// remembered value had to stand for several distinct ids. Handing the
	// replay the list instead keeps every recorded argument literal, and makes
	// the count and order of mints an assertion in their own right.
	//
	// A family the run never minted for is ABSENT rather than empty: absent
	// means unpinned, and a bundle that opens no operations should not be
	// claiming anything about how many the replay opens.
	MintedIDs map[string][]string

	// ToolCatalogs is what the run was OFFERED, per turn — one entry per
	// recorded CHANGE, in turn order, ready for the bundle's toolCatalogs.
	//
	// Carried through unchanged rather than reduced to a claim about any one
	// tool. Which capability MATTERED to a scenario is a judgement a capture
	// must not make; what the run was offered at each turn is a fact it can
	// simply state, and the replay turns the whole set into the assertion.
	//
	// Empty for a session recorded before the tool_catalog Kind shipped, which
	// pins nothing rather than claiming the run was offered nothing.
	ToolCatalogs []bt.ToolCatalog

	// UnmappedTurns are transcript indices the walk could not place. Reported
	// rather than dropped: a turn nobody folded is a hole in the replay, and
	// the self-check turns it into a hard finding.
	UnmappedTurns []int
}

// ErrorResult is one MCP tool call whose recorded error came from the UPSTREAM
// server — the call reached it and it answered with a failure.
//
// A call the platform refused is deliberately NOT one of these. That
// distinction is the whole of the classification: a denied tool never reaches
// the sandbox or MCP server (pkg/agent/runner/loop_dispatch.go), so its error
// was written by the gate and the replay's gate writes it again from the
// fixture's own seed and configuration. Emitting a canned error for it would
// make the stub answer a call nothing dispatches, and would let a bundle look
// like evidence of an upstream failure that never happened.
type ErrorResult struct {
	// TurnIndex is the transcript turn carrying the result.
	TurnIndex int
	// UseID is the tool_use block this result answers.
	UseID string
	// Tool is the LLM-facing name the transcript recorded, e.g.
	// "centerdot_list_contacts_for_company".
	Tool string
	// Server is the SERVER-side name toolOutputs and toolErrors are keyed by.
	Server string
	// Body is the error text the model was handed, unwrapped from the runner's
	// untrusted-output envelope. It becomes the JSON-RPC message the replay's
	// stub answers with; see bt.ToolError.Message for why nesting the observed
	// text is what keeps a derived Expect true.
	Body string

	// DeniedInLog records that the authz log DID deny this call, but with a
	// message that is not the one the model was handed — so the capture could
	// not prove which side authored the error.
	//
	// Such a result cans NOTHING: no toolErrors entry, no toolOutputs entry.
	// Not because the claim would be inaccurate, but because it would MASK A
	// GATE REGRESSION — the body is very likely the gate's own refusal text and
	// the step's Expect was derived from the same bytes, so a gate that stopped
	// refusing would be served that text back and satisfy the assertion meant
	// to catch it. See foldToolErrors and CodeUnprovenErrorOrigin.
	//
	// It is real rather than theoretical: the approval flow denies with
	// whatever BuildApprovalAsk's error said, and when the ask could not be
	// built at all it records nothing, leaving only an authz denial that says
	// something else.
	DeniedInLog bool
}

// Fold turns a session's transcript into bundle turns, steps and tool outputs.
//
// A pure function of its input. Every rule here is a claim about how a real
// session maps onto the replay format, and keeping it pure is what lets each
// one be tested against a synthetic transcript instead of a cluster.
//
// # Which tools contribute a canned output
//
// The MCP ones and the SANDBOX ones, and the rule is not a judgement about the
// tool — it is the shape of the replay path. A bundle replays black boxes: the
// recorded call goes in, the recorded result comes back, and the transport that
// produced it is not something the bundle reproduces. Both transports have a
// stub the driver can hand an answer to (the fake MCP server, the fake exec
// binder), so both contribute entries to the one ToolOutputs map.
//
// A META tool contributes nothing, and that is the real distinction: it executes
// for real at replay, because it IS the code under test. Whether a name is MCP
// or sandbox is read from the class's declared prefixes, never inferred from the
// name itself — an underscore in a meta tool's name would make a guess wrong in
// the direction that silently cans a tool that was supposed to run.
//
// The two differ in what has to be inverted. An MCP result is the JSON-RPC
// result verbatim. A sandbox result was COMPOSED by the sandbox tool from
// process streams plus per-run bookkeeping (artifact refs, a secret-output
// handle), so the fold recovers what the process printed and drops the rest;
// see foldSandboxResult. A composition it cannot invert is named in
// UnreplayableSandboxTools rather than papered over with a plausible entry.
func Fold(recs Records, opts FoldOptions) (Folded, error) {
	turns := slices.Clone(recs.Turns)
	slices.SortStableFunc(turns, func(a, b memory.Turn) int { return a.Index - b.Index })

	out := Folded{
		ToolOutputs:        map[string]json.RawMessage{},
		ToolOutputSequence: map[string][]json.RawMessage{},
	}

	var (
		useIDToName = map[string]string{}            // tool_use id -> LLM-facing name
		collected   = map[string][]json.RawMessage{} // SERVER-side name -> its results, in call order
		order       []string                         // first-seen order, for stable emission
		pending     bt.Expect                        // the Expect the NEXT assistant turn answers
		minted      = map[string][]string{}          // bt family -> ids the run minted, in mint order
		mintedSeen  = map[string]bool{}              // ids already recorded, so an echoed id is one mint

		// Sandbox results the fold could not take at face value, by LLM-facing
		// name. Maps rather than slices: one tool called five times reports once.
		secretOutputTools   = map[string]bool{}
		streamResultTools   = map[string]bool{}
		unreplayableSandbox = map[string]SandboxRefusal{}

		// Cannable META tools, by LLM-facing name: the bodies their SUCCESSFUL
		// calls returned, in call order, and whether any call errored. Both are
		// needed to decide the tool answered uniformly — a success and an error
		// are as much a conflict as two different successes, because one canned
		// body is served to every call.
		metaBodies  = map[string][]string{}
		metaErrored = map[string]bool{}
	)

	for _, t := range turns {
		switch {
		case t.Role == "system_note":
			// Runner metadata. Filtered from the replayed history in
			// production; promoting one to a userTurn would make the bundle
			// send a message nobody typed.
			continue

		case t.Role == "inbox" || t.Role == "inbox_done":
			// Queueing bookkeeping, not conversation — the same reason
			// system_note is skipped above. drainInbox
			// (pkg/agent/runner/loop_inbox.go) already places the message the
			// model actually saw as an ordinary "user" turn, verbatim
			// (Content: it.Content), at the index the runner assigns; the
			// `case t.Role == "user"` below folds THAT turn like any other.
			// The raw "inbox" row is kept only for audit (drainInbox's own
			// comment says so) and is never re-drained, and "inbox_done" is
			// nothing but the marker that recorded the drain happened.
			// Neither carries information the placed user turn lacks, so
			// folding them too would either duplicate the message or add a
			// hole that isn't one.
			continue

		case t.Role == "assistant" && t.Refused:
			// The runner excludes refused turns from the reconstructed history
			// so a retry regenerates cleanly. Replaying one would drive the
			// replay down a path the original run abandoned.
			continue

		case t.Role == "assistant":
			if t.Model != "" {
				out.ServedModel = t.Model
			}
			reply, err := foldReply(t, useIDToName)
			if err != nil {
				return Folded{}, fmt.Errorf("turn %d: %w", t.Index, err)
			}
			out.LLM = append(out.LLM, bt.LLMStep{Expect: pending, Reply: reply})
			pending = bt.Expect{}

		case t.Role == "user":
			if res := toolResults(t); len(res) > 0 {
				for _, r := range res {
					name := useIDToName[r.ToolUseID]
					if name == "" {
						// A result answering a call this walk never saw. The
						// tool it belongs to is unknowable, so the turn is
						// reported as a hole rather than guessed at.
						out.UnmappedTurns = appendUnmapped(out.UnmappedTurns, t.Index)
						continue
					}
					payload, _ := toolenvelope.Unwrap(r.Content)
					collectMintedIDs(minted, mintedSeen, payload)

					// A SANDBOX result is composed from process streams rather
					// than handed back verbatim, so it is inverted here and
					// filed under the same ToolOutputs map — a bundle replays
					// black boxes, and which transport answered a call is not
					// something it reproduces.
					if isSandbox, classTool := splitSandboxName(name, opts.SandboxPrefixes); isSandbox {
						// A GATE refusal contributes nothing, exactly as on the
						// MCP path below: the call never reached the sandbox,
						// and the replay's own gate refuses it again from the
						// same fixture and the same derived seed. Canning the
						// gate's own refusal text would let a gate that STOPPED
						// refusing be served that text back and satisfy the
						// assertion meant to catch it.
						if r.IsError && gateRefused(recs, r.ToolUseID, payload) {
							continue
						}
						key := sandboxOutputKey(sandboxPrefixOf(name, opts.SandboxPrefixes), classTool)
						out, kind := foldSandboxResult(payload, r.IsError)
						switch kind {
						case sandboxResultProcess, sandboxResultFailure:
						case sandboxResultSecretOutput:
							secretOutputTools[name] = true
						case sandboxResultStream:
							streamResultTools[name] = true
						default:
							refusal := unreplayableSandbox[name]
							if r.IsError {
								refusal.Errored = true
							} else {
								refusal.Succeeded = true
							}
							unreplayableSandbox[name] = refusal
							continue
						}
						if _, seen := collected[key]; !seen {
							order = append(order, key)
						}
						collected[key] = append(collected[key], out)
						continue
					}

					isMCP, server := splitMCPName(name, opts.MCPPrefixes)
					if !isMCP {
						// Not a transport, so it is a META tool — and a meta
						// tool runs for real at replay unless its own table
						// says its answer comes from state a replay cannot
						// reproduce. bt.MetaToolCannable reads that table; a
						// tool it declines contributes nothing, exactly as
						// before.
						//
						// AFTER the MCP split rather than before it, so a
						// declared transport prefix still wins: the class's own
						// mcpServers[].name is a fact about this session, while
						// the cannable table is a fact about the repo, and a
						// contrived prefix collision must not silently reroute
						// a real MCP tool's result into a canned meta reply.
						if !bt.MetaToolCannable(name) {
							continue
						}
						// An ERROR is never canned, and the two kinds of error
						// agree. A GATE refusal never reached the tool at all,
						// and canning the gate's own refusal text would let a
						// gate that STOPPED refusing be served that text back
						// and satisfy the assertion meant to catch it — the
						// same ruling the MCP branch below takes. The tool's
						// OWN refusal was composed by our code from our own
						// inputs, so the replay composes it again; canning it
						// would mask a regression in that composition.
						//
						// Recorded rather than dropped: one canned body is
						// served to EVERY call, so a tool that errored once and
						// succeeded once has no honest spelling and is refused
						// below.
						if r.IsError {
							metaErrored[name] = true
							continue
						}
						// Normalized through the tool's OWN encoder, the same
						// call expectFor makes on the same bytes. Sharing the
						// function is what keeps the canned body and the
						// expectation derived from it byte-identical: a
						// canonicalization applied to one and not the other
						// would fail every replay of a correct bundle.
						body, _ := meta.CanonicalizeResult(name, []byte(payload))
						metaBodies[name] = append(metaBodies[name], string(body))
						continue
					}
					// An ERROR result is never collected: its body is not JSON,
					// so putting it in ToolOutputs made the bundle
					// unmarshallable, and even a body that happened to be JSON
					// would replay as a success carrying the words of a
					// failure. What happens instead depends on WHO wrote it.
					//
					// A gate-generated refusal contributes nothing at all — the
					// tool never ran, and the replay's own gate refuses it
					// again. An upstream error is recorded, and becomes a
					// toolErrors entry the driver registers through
					// OnToolError. Either way Expect.LastToolResultIsError, set
					// below, still pins that the call errored.
					if r.IsError {
						if gateRefused(recs, r.ToolUseID, payload) {
							continue
						}
						out.ErrorResults = append(out.ErrorResults, ErrorResult{
							TurnIndex:   t.Index,
							UseID:       r.ToolUseID,
							Tool:        name,
							Server:      server,
							Body:        payload,
							DeniedInLog: deniedInLog(recs, r.ToolUseID),
						})
						continue
					}
					if _, seen := collected[server]; !seen {
						order = append(order, server)
					}
					collected[server] = append(collected[server], json.RawMessage(payload))
				}
				// Every result is collected and mints ids, but only ONE of them
				// describes the step: the block the driver will read.
				//
				// Skipped when that block cannot be named. It answers a call
				// this walk never saw, so nothing about it reached collected or
				// ToolOutputs and it has no route back into a replay — yet
				// checkExpect consults lastToolResultBlock on
				// LastToolResultContains or LastToolResultIsError ALONE,
				// without LastToolResult. Describing an unknowable block would
				// assert the next step against data replay cannot reproduce:
				// the same false regression as naming the wrong block, only
				// narrower.
				if useIDToName[res[0].ToolUseID] != "" {
					pending = expectFor(res[0], useIDToName, opts)
				}
				continue
			}
			if text := userText(t); text != "" {
				out.UserTurns = append(out.UserTurns, text)
				pending = bt.Expect{UserTextContains: text}
				continue
			}
			out.UnmappedTurns = appendUnmapped(out.UnmappedTurns, t.Index)

		default:
			// A role this fold has no rule for. Reported, never dropped: the
			// whole point of UnmappedTurns is that a hole in the replay is
			// visible to the self-check instead of silent.
			out.UnmappedTurns = appendUnmapped(out.UnmappedTurns, t.Index)
		}
	}

	// A tool whose successive calls all returned the same bytes stays a
	// constant; one that varied becomes a sequence. Constant is preferred so a
	// capture is no harder to read than an authored bundle.
	for _, name := range order {
		vals := collected[name]
		if allIdentical(vals) {
			out.ToolOutputs[name] = vals[0]
			continue
		}
		out.ToolOutputSequence[name] = vals
	}
	out.MetaToolReplies, out.NonUniformMetaTools = foldMetaReplies(metaBodies, metaErrored)
	out.ToolErrors, out.NonUniformErrorTools = foldToolErrors(out.ErrorResults, collected)
	out.SecretOutputTools = slices.Sorted(maps.Keys(secretOutputTools))
	out.StreamResultTools = slices.Sorted(maps.Keys(streamResultTools))
	for _, name := range slices.Sorted(maps.Keys(unreplayableSandbox)) {
		refusal := unreplayableSandbox[name]
		refusal.Tool = name
		out.UnreplayableSandboxTools = append(out.UnreplayableSandboxTools, refusal)
	}
	if len(minted) > 0 {
		// Left nil when the run minted nothing, so an unpinned family stays
		// unpinned rather than becoming an empty list the replay would then
		// have to explain.
		out.MintedIDs = minted
	}
	out.ToolCatalogs = foldToolCatalogs(recs.ToolCatalogs)
	return out, nil
}

// foldToolCatalogs carries the recorded catalog changes into bundle shape.
//
// Sorted by turn and re-sorted within each entry, because the bundle's own
// validation requires both and the replay's step function is ambiguous without
// the first. The Kind's writer already canonicalizes, so this normally changes
// nothing; doing it anyway means a bundle is well-formed even from a scope
// whose rows predate that canonicalization.
//
// The Digest is deliberately dropped. It is a pure function of Tools, and a
// bundle carrying both would be storing a value it could derive — one more
// thing that can disagree with itself in a file a human edits.
func foldToolCatalogs(recorded []toolcatalog.Content) []bt.ToolCatalog {
	if len(recorded) == 0 {
		return nil
	}
	out := make([]bt.ToolCatalog, 0, len(recorded))
	for _, c := range recorded {
		tools := slices.Clone(c.Tools)
		sort.Strings(tools)
		out = append(out, bt.ToolCatalog{FromTurnIndex: c.FromTurnIndex, Tools: tools})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FromTurnIndex < out[j].FromTurnIndex })
	return out
}

// sandboxResultKind names which composition a recorded sandbox result matched.
type sandboxResultKind int

const (
	// sandboxResultUnknown matched neither, so nothing can be recovered.
	sandboxResultUnknown sandboxResultKind = iota
	// sandboxResultProcess is the ordinary shape: stdout plus the tool's own
	// "[exit=…; artifacts: …]" trailer.
	sandboxResultProcess
	// sandboxResultSecretOutput is a producer's result, replaced wholesale by
	// the runner with a description line and an opaque handle.
	sandboxResultSecretOutput
	// sandboxResultFailure is a FAILED call whose terminal condition a replay
	// can be driven into from the exit code alone. See bt.SplitSandboxFailure.
	sandboxResultFailure
	// sandboxResultStream is a STREAMING or INTERACTIVE toolkit's composed
	// result, carried verbatim and served back as the toolkit's own output
	// rather than inverted. See bt.SplitStreamResult.
	sandboxResultStream
)

// SandboxRefusal names one sandbox tool the fold could not invert a result for,
// and what kind of result defeated it. Both flags can be set: a tool called
// several times may have produced an unrecoverable result of each kind.
type SandboxRefusal struct {
	// Tool is the LLM-facing name.
	Tool string
	// Errored is set when an ERROR result could not be inverted — a Timeout, a
	// Cancellation, an exec error, or a stderr tail the tool truncated. Only a
	// plain non-zero exit is invertible.
	Errored bool
	// Succeeded is set when a SUCCESSFUL result matched no known composition.
	//
	// A streaming or interactive toolkit's result is NOT one of these any more —
	// it is recognized and carried verbatim (sandboxResultStream). What is left
	// is a streaming result the bundle cannot honestly serve back: an idle exit,
	// whose Terminal=true ends the turn; or one over
	// bt.StreamResultBudgetBytes, which the replay's stdout tail would truncate.
	Succeeded bool
}

// foldSandboxResult inverts whichever composition produced a recorded sandbox
// result, into the bt.SandboxOutput a replay hands the fake exec binder.
//
// The per-run halves are DROPPED on both process paths and that is the point of
// doing this at all: the trailer's artifact refs embed the session name and the
// ToolCall UID, and the secret-output line's ref and byte count are equally
// per-run. Carried into a bundle either would be handed back as if the process
// had printed it, and pinned in an assertion either would fail every replay.
//
// # The streaming path is the exception, and it inverts nothing
//
// A streaming or interactive toolkit's result was never composed from process
// output, so there is no composition to invert and nothing per-run to strip: no
// durable record holds the toolkit's wire stream, and the composed text IS the
// call's only surviving artifact. It is carried whole and served back as the
// toolkit's own output. That is a real coverage loss, named in a warning rather
// than left for a reader to infer — see bt.SandboxOutput.StreamResult and
// CodeStreamResultServedVerbatim.
//
// # The failure path
//
// isError selects it, from the record rather than from the text. A non-zero
// exit makes the toolcall controller mark the ToolCall Failed, so the result is
// a summary of the terminal CONDITION with no stdout in it — and what the
// bundle expresses is therefore the exit code, which is the one lever a
// replayed ToolCall can be driven into that condition with. Nothing is
// invented: the exit code and the stderr tail are both recovered from the
// recorded text, and the replay's own sandbox tool re-composes the summary
// line. bt.SplitSandboxFailure refuses every condition an exit code cannot
// reach, so a Timeout, a Cancellation and an exec error all stay unknown.
//
// A gate refusal never reaches here — the caller checks gateRefused first, for
// the reason gateRefused's own comment gives.
func foldSandboxResult(payload string, isError bool) (json.RawMessage, sandboxResultKind) {
	if isError {
		if stderr, exit, ok := bt.SplitSandboxFailure(payload); ok {
			return marshalSandboxOutput(bt.SandboxOutput{Stderr: stderr, ExitCode: exit}), sandboxResultFailure
		}
		// A failed call's text is neither of the SUCCESS shapes below, and
		// trying them would at best recover something the model never saw.
		return nil, sandboxResultUnknown
	}
	if stdout, _, ok := bt.SplitSandboxResult(payload); ok {
		return marshalSandboxOutput(bt.SandboxOutput{Stdout: stdout}), sandboxResultProcess
	}
	if desc, ok := bt.SplitSecretOutputResult(payload); ok {
		// The description is what the tool's stdout becomes at replay, because
		// RewriteFixture drops spec.secretOutput from the emitted toolspec and
		// the replayed call therefore composes an ordinary process result. For a
		// `file:`-sourced output that description IS the producer's real stdout.
		return marshalSandboxOutput(bt.SandboxOutput{Stdout: desc + "\n"}), sandboxResultSecretOutput
	}
	if composed, ok := bt.SplitStreamResult(payload); ok {
		return marshalSandboxOutput(bt.SandboxOutput{StreamResult: composed}), sandboxResultStream
	}
	return nil, sandboxResultUnknown
}

// marshalSandboxOutput renders a SandboxOutput as the ToolOutputs value.
//
// json.Marshal of a struct with no interface fields and no channels cannot
// fail, so the error is discarded here rather than threaded through every
// caller — the alternative is an error return nobody can produce and no test
// can cover.
func marshalSandboxOutput(so bt.SandboxOutput) json.RawMessage {
	raw, _ := json.Marshal(so)
	return raw
}

// sandboxPrefixOf returns which of prefixes an LLM-facing name carries.
// Empty when none does; callers reach it only after splitSandboxName said one
// matched.
func sandboxPrefixOf(llmFacing string, prefixes []string) string {
	for _, p := range prefixes {
		if strings.HasPrefix(llmFacing, p+"_") {
			return p
		}
	}
	return ""
}

// gateRefused reports whether an ERROR tool result was written by a gate that
// refused the call BEFORE it was dispatched.
//
// This is the classification the whole error-result question turns on. A denied
// tool never reaches the sandbox or MCP server (pkg/agent/runner/
// loop_dispatch.go), so such a result needs nothing canned — the replay boots
// the same fixture with the same derived seed and the same gate configuration
// and refuses it again. Canning one instead would make the fake server answer a
// call nothing dispatches, and would dress a platform refusal up as evidence of
// an upstream failure.
//
// # It FAILS CLOSED
//
// Only two per-call records can PROVE a pre-dispatch refusal, and anything else
// is read as upstream. That is the safe direction: reading an upstream error as
// gate-generated emits a bundle whose replay serves the call to SUCCESS,
// silently reversing what the capture is evidence of, while reading a gate
// refusal as upstream at worst cans an error the gate pre-empts before the stub
// is ever reached.
//
//   - A DENIED approval for this tool_use id. A refusal — an explicit deny, a
//     timeout, or a post-approval authorization re-check that failed — becomes
//     a PreToolCall Deny, and the call never runs. The outcome record alone is
//     the proof; no content comparison is possible, because the message the
//     model saw is the approval host's anti-confabulation framing and no record
//     holds it.
//   - A DENIED authz decision for this tool_use id whose recorded Message is
//     exactly the body the model was handed. The message anchor is what makes
//     this a proof rather than an inference: a denied check does NOT always
//     stop the call — under a permissive tool-call mode a readonly/readwrite
//     denial is logged and the dispatch proceeds — so the denial's existence
//     says nothing on its own. The bytes do: an authz Deny hands back
//     res.Message verbatim, and that is the same string recordAuthzDecision
//     stored.
//
// # What is deliberately NOT a signal
//
// The plan-gate log records a per-call Outcome and a UseID, and looks like it
// should serve. It does not: plangate.go stamps OutcomeDenied on the record and
// then returns Allow with an approval ask, so a gate record marked denied can
// belong to a call a human went on to approve and the tool went on to run.
// Those calls are covered by the approval signal above when they were refused,
// and are correctly upstream-classified when they were not.
//
// The LAST decision for a call decides, not the first. Denied-then-approved
// re-checks the same tool_use id and records both; the allow is what dispatched
// it, so an error that call carried came from the server.
func gateRefused(recs Records, useID, body string) bool {
	if useID == "" {
		// A result the transcript could not attribute to a call correlates to
		// no record at all. Nothing can be proven about it.
		return false
	}
	if pair, ok := recs.Approvals[useID]; ok && pair.Outcome != nil && pair.Outcome.Decision == approvalDenied {
		return true
	}
	decisions := recs.DecisionsByToolCall[useID]
	if len(decisions) == 0 {
		return false
	}
	last := decisions[len(decisions)-1]
	// A denial that recorded no reason cannot be shown to have authored
	// anything: an empty Message would match an empty body, and every
	// unexplained empty error would read as gate-generated.
	return last.Outcome == authzdecision.OutcomeDenied && last.Message != "" && last.Message == body
}

// deniedInLog reports whether the authz log recorded a DENIAL as this call's
// last outcome — asked only about a result gateRefused has already declined to
// classify, so it means "denied, but not with the message the model saw."
//
// See ErrorResult.DeniedInLog for why that combination is worth carrying rather
// than silently emitting a toolErrors entry that misdescribes its own origin.
func deniedInLog(recs Records, useID string) bool {
	decisions := recs.DecisionsByToolCall[useID]
	if len(decisions) == 0 {
		return false
	}
	return decisions[len(decisions)-1].Outcome == authzdecision.OutcomeDenied
}

// approvalDenied is the outcome string the runner records for a refused
// approval (pkg/agent/runner/host_approval.go). Spelled here because
// approval.Outcome.Decision is a documented-by-comment string with no exported
// constants; a mismatch would silently classify every human refusal as an
// upstream error.
const approvalDenied = "denied"

// foldMetaReplies turns the collected cannable-meta bodies into the map the
// bundle carries, and names the tools that map cannot honestly represent.
//
// A tool is representable only when EVERY call answered the same way: one
// bt.MetaToolReply is one body, served on every call, so two different
// successes have no spelling — and neither does a success beside an error,
// because canning the success would serve it to the call that errored while the
// step derived from that error asserted something else entirely.
//
// A tool WITH a conflict still gets its entry, exactly as foldToolErrors leaves
// one: the capture is refused anyway, so the value is never used, and leaving
// the key out would make a reader diffing the findings against the emitted file
// wonder which tool the finding meant.
//
// A tool whose calls ALL errored contributes no entry and no conflict. Nothing
// is canned, the replay's own tool composes its own refusal, and there is no
// bundle field that could say otherwise.
func foldMetaReplies(bodies map[string][]string, errored map[string]bool) (map[string]bt.MetaToolReply, []string) {
	if len(bodies) == 0 {
		return nil, nil
	}
	out := make(map[string]bt.MetaToolReply, len(bodies))
	var conflicted []string
	for _, name := range slices.Sorted(maps.Keys(bodies)) {
		vals := bodies[name]
		out[name] = bt.MetaToolReply{Content: vals[0]}
		if errored[name] || !allIdenticalStrings(vals) {
			conflicted = append(conflicted, name)
		}
	}
	return out, conflicted
}

// allIdenticalStrings reports whether every body is byte-identical to the
// first. A single body is trivially uniform; an empty slice never reaches here.
func allIdenticalStrings(vals []string) bool {
	for _, v := range vals[1:] {
		if v != vals[0] {
			return false
		}
	}
	return true
}

// foldToolErrors turns the per-call upstream errors into the map the bundle
// carries, and names the tools that map cannot honestly represent.
//
// A tool is representable only when every call of it that reached the server
// failed the SAME way. MCPStub.OnToolError is keyed by tool name, wins over
// OnTool for every call, and is not counted, so there is no per-call variation
// available: a tool that errored once and succeeded once, or errored twice with
// different text, has no spelling at all. Those are returned as names rather
// than dropped — the self-check refuses the capture on them.
//
// A tool WITH a conflict still gets its entry. The bundle is refused anyway, so
// the value is never used; leaving the key out would make a reader diffing the
// findings against the emitted file wonder which tool the finding meant.
func foldToolErrors(errs []ErrorResult, collected map[string][]json.RawMessage) (map[string]bt.ToolError, []string) {
	if len(errs) == 0 {
		return nil, nil
	}
	out := make(map[string]bt.ToolError, len(errs))
	bodies := map[string]string{}
	conflicted := map[string]bool{}
	for _, er := range errs {
		// An error whose ORIGIN could not be proven cans nothing at all, and
		// this is a safety property rather than a nicety about accuracy. Its
		// body is very likely the gate's own refusal, and Expect for that step
		// was derived from the same bytes — so a canned entry would be served
		// by a REGRESSED gate (one that no longer refuses, so the call reaches
		// the stub) and would satisfy the very assertion meant to catch it. The
		// bundle would pass with the permission boundary broken.
		//
		// Withholding it is strictly safer in both directions: a working gate
		// still produces the real refusal and the Expect matches, while a
		// broken one reaches a tool with no error registered and cannot
		// reproduce the refusal text, so the step fails and names itself.
		//
		// It is also why such a result takes no part in the conflict below: an
		// entry that is never made cannot disagree with a success about it, so
		// a denied-then-approved session carrying an approval-flow refusal is
		// no longer refused as tool-error-not-uniform.
		if er.DeniedInLog {
			continue
		}
		if seen, ok := bodies[er.Server]; ok && seen != er.Body {
			conflicted[er.Server] = true
		}
		bodies[er.Server] = er.Body
		// The observed text becomes the JSON-RPC message. The client renders
		// that message into the tool result the replay records, so the original
		// text is NESTED inside the replayed one — which is what a derived
		// Expect needs, since LastToolResultContains is a substring check. The
		// numeric code is not observable at all; see bt.ToolError.Code.
		out[er.Server] = bt.ToolError{Message: er.Body}
	}
	if len(out) == 0 {
		// Every error was unproven, so nothing is canned. nil rather than an
		// allocated empty map, matching the no-errors return above: a Folded
		// built by hand in a test and one folded from a session that canned
		// nothing must be the same value.
		return nil, nil
	}
	for server := range out {
		if len(collected[server]) > 0 {
			conflicted[server] = true
		}
	}
	if len(conflicted) == 0 {
		return out, nil
	}
	return out, slices.Sorted(maps.Keys(conflicted))
}

// expectFor describes the tool_result block the DRIVER will read when it checks
// this step for divergence.
//
// That block is the FIRST of the turn, not the last, despite the driver's
// helpers being named lastToolResultBlock / lastToolResultName. Both walk
// req.Messages BACKWARD but each message's blocks FORWARD and return on the
// first hit, so what they return is the first tool_result of the NEWEST
// message. Fold matches its consumer.
//
// A turn carries more than one tool_result only when the model made PARALLEL
// tool calls: the runner packs all N results into a single memory.Turn in
// tool_use order. No authored bundle has ever emitted more than one toolUse per
// reply, so nothing has exercised this — and a capture is the first producer
// that meets it routinely. Describing the last block instead would fail three
// checkExpect assertions on a replay that took the IDENTICAL path, a false
// regression on the one field this whole feature exists to make trustworthy.
func expectFor(r *memory.ToolResultBlock, useIDToName map[string]string, opts FoldOptions) bt.Expect {
	payload, _ := toolenvelope.Unwrap(r.Content)
	name := useIDToName[r.ToolUseID]

	// A SANDBOX result carries per-run bookkeeping the replay cannot reproduce:
	// the tool appends "[exit=…; artifacts: stdout=<ref>, stderr=<ref>]", and
	// those refs embed the session name and the ToolCall UID. Pinning the whole
	// payload would fail EVERY replay of a correct bundle, reading as a
	// regression in the tool rather than as an assertion nobody could satisfy.
	// So the claim is narrowed to the half that IS reproduced — what the process
	// printed — and dropped entirely when even that cannot be recovered.
	isSandbox, _ := splitSandboxName(name, opts.SandboxPrefixes)
	if isSandbox && !r.IsError {
		switch stdout, _, ok := bt.SplitSandboxResult(payload); {
		case ok:
			payload = stdout
		default:
			// A producer's result is a description line plus an opaque handle;
			// the handle and its byte count are per-run for the same reason.
			if desc, isSecretOut := bt.SplitSecretOutputResult(payload); isSecretOut {
				payload = desc
				break
			}
			// A STREAMING toolkit's composed result is served back whole, so
			// the whole of it is what the replay's own composition wraps and
			// the assertion can name every byte of it. Nothing per-run to
			// strip: the duration and the cost are frozen in the bundle rather
			// than recomputed, and the replay's own header sits ABOVE this text
			// rather than replacing it.
			if composed, isStream := bt.SplitStreamResult(payload); isStream {
				payload = composed
				break
			}
			payload = "" // no shape matched, which asserts nothing
		}
	} else if canon, ok := meta.CanonicalizeResult(name, []byte(payload)); ok {
		// A META tool's result, re-expressed through that tool's OWN encoder.
		//
		// Same move as the canned branch below and for the same reason: the
		// claim must be what the replay WILL produce, not the bytes that
		// happened to be recorded, whenever the difference between them is not
		// a difference in behaviour. There it is the fake server's encoder;
		// here it is the tool's, which is the stronger of the two — a meta tool
		// composes its reply for real at replay, so its own encoder is exactly
		// what those bytes will come out of.
		//
		// It exists because the sanitizer emitted its warnings in Go map order
		// until that was fixed, so every session recorded before the fix pinned
		// one arbitrary permutation of a set the tool now emits sorted. Pinning
		// it would be asserting bytes current code cannot produce.
		//
		// Values are untouched — see meta.CanonicalizeResult, which refuses a
		// payload carrying a field its type does not know rather than dropping
		// it, so the normalization can only reorder.
		//
		// BEFORE the canned branch, which claims any decodable JSON: both use
		// encoding/json and so agree on HTML escaping, but only this one knows
		// the result's own type and its canonical order.
		payload = string(canon)
	} else if served, ok := bt.ServedResult(json.RawMessage(payload)); ok && !cannedVerbatim(name, r.IsError) {
		// A canned result reaches the model through the fake MCP server's own
		// encoder, which is not a passthrough: it rewrites <, > and & inside
		// strings. Asserting on the recorded text would then be asserting on
		// bytes the replay cannot produce — a divergence report on a run that
		// took the identical path. So the claim is what WILL be served, derived
		// from the one function the replay's handler is built on.
		//
		// Excluded for a sandbox result, whose payload above is recovered plain
		// stdout the exec binder hands back unwrapped, and harmless for a META
		// tool that RUNS at replay: its own encoder is encoding/json too, so it
		// re-emits the same escaping this would apply.
		//
		// Excluded again for a meta reply this bundle CANS — see
		// cannedVerbatim. There the reply reaches the model as the recorded
		// bytes with no encoder in the path at all, so escaping the assertion
		// would pin bytes the replay cannot produce.
		payload = string(served)
	}

	// isError is SET, including to false. Left nil it would mean "nobody
	// checked", and a bundle asserting only on the agent's words stays green
	// while every call is denied. Copied rather than pointed at the record:
	// the turns are cloned SHALLOWLY, so a pointer into r would alias the
	// caller's own memory.Turn.
	isErr := r.IsError
	return bt.Expect{
		LastToolResult:         name,
		LastToolResultContains: payload,
		LastToolResultIsError:  &isErr,
	}
}

// cannedVerbatim reports whether this result is one the fold CANS, and which
// the replay therefore hands the model byte for byte.
//
// The one question expectFor has to ask before applying the fake MCP server's
// encoder to a meta result. A canned reply is served by the canner as a plain
// tool.Result — no JSON-RPC round trip, no re-encoding — so the assertion
// derived from it must be the recorded bytes and nothing else. A meta tool that
// RUNS goes through its own encoding/json and agrees with that encoder anyway,
// which is why the exclusion is this narrow.
//
// The error half mirrors the fold's canning branch exactly: an error is never
// canned, so an error result is still normalized like any other.
func cannedVerbatim(name string, isError bool) bool {
	return !isError && bt.MetaToolCannable(name)
}

// foldReply maps one assistant turn's blocks onto the reply parts a bundle
// spells, recording each tool_use id so the result answering it can name its
// tool later.
//
// The two text shapes are distinct and both real. A tool_use of
// respond_to_user is a message to a person and becomes ReplyPart.Text; a plain
// text block is narration the model emitted alongside a call and becomes
// ReplyPart.BareText. Promoting narration to Text would make the bundle send a
// reply the original run never sent.
//
// # Which shape a respond_to_user takes
//
// bt.ReplyPart.Text is SHORTHAND for a text-only respond_to_user: the driver
// turns it back into exactly that call and nothing else. So it fits only a call
// whose arguments are exactly one non-empty text, and every other
// respond_to_user is emitted as an ordinary bt.ReplyPart.ToolUse carrying its
// full arguments verbatim — which is what makes a reply that DELIVERED an
// artifact replayable rather than silently reduced to its words.
//
// The shorthand is kept for the common case rather than collapsing everything
// onto ToolUse: it is what a reader of the bundle expects to see, and it is
// what DeriveAssertions' AgentReplyContains and the driver's own reply
// rendering are both written around.
func foldReply(t memory.Turn, useIDToName map[string]string) ([]bt.ReplyPart, error) {
	var parts []bt.ReplyPart
	for _, b := range t.Content {
		switch {
		case b.Type == "tool_use" && b.ToolUse != nil:
			useIDToName[b.ToolUse.ID] = b.ToolUse.Name

			if b.ToolUse.Name == respondToUserToolName {
				text, only, err := respondTextOnly(b.ToolUse.Input)
				if err != nil {
					return nil, fmt.Errorf("tool_use %s: %w", b.ToolUse.ID, err)
				}
				if only {
					parts = append(parts, bt.ReplyPart{Text: text})
					continue
				}
				// Falls through to the ordinary tool_use path, which
				// round-trips the call with every argument intact.
			}

			args, err := foldArgs(b.ToolUse.Input)
			if err != nil {
				return nil, fmt.Errorf("tool_use %s (%s): %w", b.ToolUse.ID, b.ToolUse.Name, err)
			}
			parts = append(parts, bt.ReplyPart{ToolUse: &bt.ToolUse{Name: b.ToolUse.Name, Args: args}})

		case b.Type == "text" && b.Text != "":
			parts = append(parts, bt.ReplyPart{BareText: b.Text})
		}
	}
	return parts, nil
}

// respondTextOnly reports whether a respond_to_user call is one the Text
// shorthand can carry WHOLE, and returns the message if so.
//
// The test is deliberately strict: exactly one argument, named text, holding a
// non-empty string. Anything else — a second argument such as `attached`, a
// text that is not a string, no arguments at all — is a call the shorthand
// cannot reproduce, and foldReply emits it as a full tool_use instead.
//
// The empty and missing cases go the same way for a reason that is not
// symmetry. The driver's replyParts switch matches on `p.Text != ""`, so a
// ReplyPart carrying an empty Text contributes NO reply block at all: the
// replay would silently make one call fewer than the run did. Emitting the call
// as it was is both faithful and self-correcting — the replay makes the same
// malformed call and is handed the same argument error.
//
// A body that is not JSON is an error rather than "not text-only", so the
// caller refuses the transcript instead of quietly re-encoding bytes it could
// not read.
func respondTextOnly(input []byte) (string, bool, error) {
	if len(input) == 0 {
		return "", false, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(input, &args); err != nil {
		return "", false, fmt.Errorf("decoding respond_to_user args: %w", err)
	}
	raw, ok := args["text"]
	if !ok || len(args) != 1 {
		return "", false, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || text == "" {
		return "", false, nil
	}
	return text, true, nil
}

// foldArgs canonicalizes a captured call's arguments for the bundle.
//
// Passed through unchanged in VALUE — an id the run minted stays literal,
// because the replay mints the same one back (see Folded.MintedIDs). Decoded
// and re-encoded only so the emitted JSON is canonical, and so a body that is
// not JSON is refused here rather than producing a bundle nothing can load.
func foldArgs(input []byte) (json.RawMessage, error) {
	if len(input) == 0 {
		return nil, nil
	}
	var args any
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("decoding tool args: %w", err)
	}
	out, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("re-encoding tool args: %w", err)
	}
	return out, nil
}

// collectMintedIDs records every minted id a tool result carried, filed by
// family and in the order the result presents them.
//
// # Why by SHAPE rather than by field name
//
// A minted id is recognizable on its own: a family prefix plus the fixed-width
// hex body every mint site emits (bt.FamilyOf). Reading named fields instead
// worked only for the tools that put the id at a result's top level.
// update_plan puts one per item inside its item array, and naming the path to
// them would be a rule per tool rather than a fact about ids.
//
// # Why order and dedup are the whole contract
//
// The replay draws from this list in order, so it must be a faithful mint log:
//
//   - arrays are walked in order and objects by SORTED KEY, so two folds of one
//     transcript produce the same list — a capture is a reproducibility
//     artifact;
//   - an id already recorded is skipped, so a later tool echoing one back is
//     not counted as a second mint.
//
// The payload is sliced to its outermost braces because a result may be prose
// around an object. One that is not JSON records nothing, which is the ordinary
// case rather than a failure: most tool output mints nothing.
func collectMintedIDs(minted map[string][]string, seen map[string]bool, payload string) {
	start, end := strings.Index(payload, "{"), strings.LastIndex(payload, "}")
	if start < 0 || end <= start {
		return
	}
	var body any
	if err := json.Unmarshal([]byte(payload[start:end+1]), &body); err != nil {
		return
	}
	for _, id := range mintedIDsIn(body) {
		if seen[id] {
			continue
		}
		seen[id] = true
		family, _ := bt.FamilyOf(id) // non-empty: mintedIDsIn only returns matches
		minted[family] = append(minted[family], id)
	}
}

// mintedIDsIn walks a decoded result and returns every string that IS a minted
// id, in a deterministic order: arrays in order, objects by sorted key.
//
// Whole-string matching, which is what keeps a tool's own error message ("…
// op-… is not registered") from being read as a mint. Recording a mint that
// never happened is worse than missing one: it shifts every later id in the
// family by a position.
func mintedIDsIn(v any) []string {
	switch val := v.(type) {
	case string:
		if _, ok := bt.FamilyOf(val); ok {
			return []string{val}
		}
		return nil
	case map[string]any:
		var out []string
		for _, k := range slices.Sorted(maps.Keys(val)) {
			out = append(out, mintedIDsIn(val[k])...)
		}
		return out
	case []any:
		var out []string
		for _, elem := range val {
			out = append(out, mintedIDsIn(elem)...)
		}
		return out
	default:
		return nil
	}
}

// splitMCPName reports whether an LLM-facing name resolved to a declared MCP
// server, and returns the SERVER-side name to key toolOutputs by.
//
// Getting this backwards fails at replay as "unknown tool" rather than as
// anything mentioning the mapping, which is why it is one named function
// rather than an inline strings.TrimPrefix at each call site.
func splitMCPName(llmFacing string, prefixes []string) (bool, string) {
	for _, p := range prefixes {
		if after, ok := strings.CutPrefix(llmFacing, p+"_"); ok {
			return true, after
		}
	}
	return false, llmFacing
}

// toolResults returns the turn's tool_result blocks in order.
func toolResults(t memory.Turn) []*memory.ToolResultBlock {
	var out []*memory.ToolResultBlock
	for _, b := range t.Content {
		if b.Type == "tool_result" && b.ToolResult != nil {
			out = append(out, b.ToolResult)
		}
	}
	return out
}

// userText joins the turn's text blocks into the message a person typed.
func userText(t memory.Turn) string {
	var parts []string
	for _, b := range t.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// appendUnmapped records a turn index once, however many of its blocks failed
// to place. The unit a reader acts on is the turn, and repeating an index would
// make the self-check's finding read as several holes rather than one.
func appendUnmapped(dst []int, idx int) []int {
	if len(dst) > 0 && dst[len(dst)-1] == idx {
		return dst
	}
	return append(dst, idx)
}

func allIdentical(vals []json.RawMessage) bool {
	for _, v := range vals[1:] {
		if string(v) != string(vals[0]) {
			return false
		}
	}
	return true
}
