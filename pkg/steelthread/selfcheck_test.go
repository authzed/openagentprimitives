package steelthread_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// liveTokenValue is the live secret material cleanCapture claims the capture
// read. A made-up value, and deliberately distinctive: the secret-leak check
// looks for a literal byte match, so a value that could plausibly appear in an
// unrelated fixture would make the negative control meaningless.
const liveTokenValue = "xoxb-fixture-only-000111222333"

// findByCode returns the finding carrying code, failing the test with the full
// set when it is absent — so a check that never fired reports as "code X not
// found among [...]" rather than as an index panic three lines later.
func findByCode(t *testing.T, findings []steelthread.Finding, code string) steelthread.Finding {
	t.Helper()
	var codes []string
	for _, f := range findings {
		if f.Code == code {
			return f
		}
		codes = append(codes, f.Code)
	}
	require.Failf(t, "expected finding not raised", "code %q not found among %v", code, codes)
	return steelthread.Finding{}
}

// cleanRecords is the transcript shape every capture produces and nothing in
// this file's checks should object to: a person asks, the model calls one
// declared MCP tool, gets a result, and answers through respond_to_user.
func cleanRecords(t *testing.T) steelthread.Records {
	t.Helper()
	return steelthread.Records{
		Session: "default/demo-session",
		Turns: []memory.Turn{
			userText(0, "list the widgets"),
			assistantCall(1, "tu_1", "acme_list_widgets", `{"operation_id":"op-1","_reason":"asked","args":{}}`),
			toolResult(2, "tu_1", `{"results":[]}`, false),
			assistantCall(3, "tu_2", "respond_to_user", `{"text":"no widgets found"}`),
		},
	}
}

// cleanCapture is the negative control's input, and the base every doctored
// case starts from. Folded is produced by the real Fold rather than
// hand-assembled, so a case that doctors a transcript turn and a case that
// doctors a folded step are both measured against the same starting point.
func cleanCapture(t *testing.T) steelthread.SelfCheckInput {
	t.Helper()
	recs := cleanRecords(t)
	folded, err := steelthread.Fold(recs, steelthread.FoldOptions{MCPPrefixes: []string{"acme"}})
	require.NoError(t, err, "the clean fixture must fold cleanly, or every case below starts from a broken base")

	in := steelthread.SelfCheckInput{
		Records: recs,
		Folded:  folded,
		// The assembled bundle, in the shape Capture builds it from this same
		// fold: checkBundle runs the replay driver's load preconditions over
		// it, and a zero value fails them, so the negative control needs a real
		// one or every case below reports bundle-invalid.
		Bundle: bt.Bundle{
			Name:       "demo-capture",
			AgentDir:   "testdata/demo-capture",
			AgentClass: "demo-agent",
			UserTurns:  bundleTurns(folded.UserTurns),
			LLM:        folded.LLM,
		},
		DeclaredTools: map[string][]string{"acme": {"list_widgets"}},
		MetaTools:     []string{"update_plan", "agent_work_complete"},
		// The fixture's own assembly, and in the clean case it agrees with the
		// live one: nothing this fixture rewrites contributes a meta tool. The
		// cases that make them DISAGREE are what checkFixtureTools reports.
		FixtureTools: []steelthread.FixtureTool{
			{Name: "update_plan"}, {Name: "agent_work_complete"},
		},
		LiveSecrets: []steelthread.LiveSecret{
			{Name: "demo-agent-creds/bot-token", Value: liveTokenValue},
		},
		// The placeholder in 00-secret.yaml stands in for a live Secret of this
		// name, which is what makes LiveSecrets REQUIRED here — and what the
		// entry above satisfies. See RewriteResult.ShadowedSecrets.
		ShadowedSecrets: []string{"demo-agent-creds"},
		Files: []steelthread.FixtureFile{
			{Name: "00-secret.yaml", YAML: []byte("apiVersion: v1\nkind: Secret\nstringData:\n  bot-token: unused-by-the-fake-channel\n")},
			{Name: "03-agent.yaml", YAML: []byte("apiVersion: spicebox.dev/v1alpha1\nkind: AgentClass\nmetadata:\n  name: demo-agent\n")},
		},
	}
	in.Emitted = emittedFrom(t, in.Bundle, in.Files)
	return in
}

// emittedFrom mirrors what Capture hands the self-check: the marshalled
// bundle plus every fixture manifest, as the FINAL bytes.
//
// Derived here rather than hand-written so the two lists cannot disagree. Both
// secret scans read Emitted, so a fixture that populated Files alone would
// exercise nothing at all — which is the shape of the gap this surface exists
// to close, and it would be embarrassing to reintroduce it in the tests.
func emittedFrom(t *testing.T, b bt.Bundle, files []steelthread.FixtureFile) []steelthread.EmittedFile {
	t.Helper()
	raw, err := json.MarshalIndent(b, "", "  ")
	require.NoError(t, err, "the fixture's bundle must marshal, or Emitted cannot be built")
	out := []steelthread.EmittedFile{{Name: "bundle.json", Bytes: append(raw, '\n')}}
	for _, f := range files {
		out = append(out, steelthread.EmittedFile{Name: f.Name, Bytes: f.YAML})
	}
	return out
}

// plant appends bytes to one emitted file, and to the fixture manifest of the
// same name when there is one.
//
// Both, because Capture emits one set of bytes per manifest and hands the same
// bytes to both fields. A test doctoring only Emitted would leave the
// Secret-bearing gate reading a manifest the capture never wrote.
func plant(t *testing.T, in *steelthread.SelfCheckInput, name string, extra []byte) {
	t.Helper()
	var found bool
	for i := range in.Emitted {
		if in.Emitted[i].Name != name {
			continue
		}
		in.Emitted[i].Bytes = append(slices.Clone(in.Emitted[i].Bytes), extra...)
		found = true
	}
	require.True(t, found, "no emitted file named %q to plant into", name)
	for i := range in.Files {
		if in.Files[i].Name == name {
			in.Files[i].YAML = append(slices.Clone(in.Files[i].YAML), extra...)
		}
	}
}

// TestSelfCheck covers every finding. Table-driven: the cases share one shape
// (a clean capture with exactly one thing doctored in, an expected finding code
// out) and differ only in which invariant is broken.
func TestSelfCheck(t *testing.T) {
	cases := []struct {
		name     string
		doctor   func(t *testing.T, in *steelthread.SelfCheckInput)
		wantCode string
		wantSev  steelthread.Severity
		names    string // a substring the message MUST contain
	}{
		{
			// A declared tool the transcript never called. The assembly step
			// gives it the placeholder entry class admission needs — the same
			// one a human writes into an authored bundle — and the reader is
			// TOLD, so an invented entry is distinguishable from a recorded
			// one. A warning, because nothing is wrong: refusing here is what
			// used to disqualify almost every real session.
			name: "a declared tool the transcript never called",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.DeclaredTools["acme"] = append(in.DeclaredTools["acme"], "archive_widget")
				in.PlaceholderTools = []string{"archive_widget"}
			},
			wantCode: steelthread.CodePlaceholderToolOutputs,
			wantSev:  steelthread.SeverityWarn,
			names:    "archive_widget",
		},
		{
			name: "a turn that mapped to no step",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Folded.UnmappedTurns = []int{4}
			},
			wantCode: steelthread.CodeUnmappedTurn,
			wantSev:  steelthread.SeverityHard,
			names:    "4",
		},
		{
			// THE regression this check exists for. The run was offered a
			// channel-sourced meta tool; the fixture rewrites its Channel to
			// kind=fake, whose assembly does not produce it. Before this the
			// bundle emitted with zero hard findings and died minutes later in
			// the replay's own catalog check, naming the driver.
			name: "a tool the run was offered that the fixture will not offer",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Folded.ToolCatalogs = []bt.ToolCatalog{
					{FromTurnIndex: 1, Tools: []string{"agent_work_complete", "lookup_user_for_mention", "update_plan"}},
				}
			},
			wantCode: steelthread.CodeOfferedToolNotProducible,
			wantSev:  steelthread.SeverityHard,
			names:    "lookup_user_for_mention",
		},
		{
			// The gate, and it is the whole reason the case above can be
			// trusted: an unsupplied prediction must not read as "the fixture
			// offers nothing" and refuse every tool by name.
			name: "a recorded catalog with no prediction to compare it against",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Folded.ToolCatalogs = []bt.ToolCatalog{
					{FromTurnIndex: 1, Tools: []string{"agent_work_complete", "update_plan"}},
				}
				in.FixtureTools = nil
			},
			wantCode: steelthread.CodeFixtureToolsNotComputed,
			wantSev:  steelthread.SeverityHard,
			names:    "ReplayChannel",
		},
		{
			// A meta tool reaching the input Channel kind's own provider, on a
			// capture that read no revision back out of the records. The
			// surface reads the triggering resource before anything else, so a
			// stand-in with nothing to report refuses every such call.
			name: "a call to a provider-surface tool with no revision to seed",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Records.Turns = append(in.Records.Turns,
					assistantCall(4, "tu_3", "claim_review_status", `{"summary":"looks fine"}`))
				// Named in BOTH lists: it is a meta tool, so checkToolCalls has
				// no complaint about it, and the only thing wrong with it is
				// that nothing was recovered to seed its provider with.
				in.MetaTools = append(in.MetaTools, "claim_review_status")
				in.FixtureTools = append(in.FixtureTools,
					steelthread.FixtureTool{Name: "claim_review_status", ExternalSurface: true})
				in.Bundle.Trigger = &bt.Trigger{Channel: "demo-chat", Payload: "delivery.json",
					Event: "pull_request", ChannelKey: "pr:demo-org/platform#42"}
			},
			wantCode: steelthread.CodeProviderStateUnseeded,
			wantSev:  steelthread.SeverityHard,
			names:    "claim_review_status",
		},
		{
			// The GATE on the case above, and the reason it can be trusted: an
			// empty seed because nobody resolved the kind's reporter must not
			// read as "the run observed nothing".
			name: "a call to a provider-surface tool with no reporter supplied",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Records.Turns = append(in.Records.Turns,
					assistantCall(4, "tu_3", "claim_review_status", `{"summary":"looks fine"}`))
				in.MetaTools = append(in.MetaTools, "claim_review_status")
				in.FixtureTools = append(in.FixtureTools,
					steelthread.FixtureTool{Name: "claim_review_status", ExternalSurface: true})
				in.TriggerProviderMissing = true
			},
			wantCode: steelthread.CodeProviderStateNotComputed,
			wantSev:  steelthread.SeverityHard,
			names:    "CaptureInput.TriggerProvider",
		},
		{
			// A tool the fixture can only OFFER through a seeded stand-in. The
			// offer is reproduced faithfully; the REPLY is the stand-in kind's
			// own rendering and is not the one the step's expectation was
			// derived from.
			name: "a call to a tool the fixture can only offer through a stand-in",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Records.Turns = append(in.Records.Turns,
					assistantCall(4, "tu_3", "lookup_user_for_mention", `{"kind":"email","value":"dana@example.test"}`))
				in.MetaTools = append(in.MetaTools, "lookup_user_for_mention")
				in.FixtureTools = append(in.FixtureTools,
					steelthread.FixtureTool{Name: "lookup_user_for_mention", StoodIn: true})
			},
			wantCode: steelthread.CodeStoodInToolCalled,
			wantSev:  steelthread.SeverityHard,
			names:    "lookup_user_for_mention",
		},
		{
			// An id in this repo's own minting shape that no seam reproduces.
			// The meta tool RUNS at replay, so every other byte of its reply is
			// composed by our code — but this one is minted afresh, and the
			// step's expectation was derived from the recorded value.
			name: "a meta tool result naming an unpinned minted-shape id",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.MetaTools = append(in.MetaTools, "artifact_prepare")
				in.Folded.LLM = append(in.Folded.LLM, bt.LLMStep{Expect: bt.Expect{
					LastToolResult:         "artifact_prepare",
					LastToolResultContains: `{"revision_id":"artrev-171fcb3ee98f70b3","seq":1}`,
				}})
			},
			wantCode: steelthread.CodeUnreproducibleID,
			wantSev:  steelthread.SeverityHard,
			names:    "artrev-171fcb3ee98f70b3",
		},
		{
			// A credential whose type MINTS and which no harness serves. The
			// fixture emits a placeholder Secret beside it, the replayed
			// identity goes Valid on that, and every call drawing on the
			// credential fails at dispatch.
			name: "a fixture credential whose type mints and no harness serves",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.MintedCredentials = []steelthread.MintedCredential{
					{Label: "demo-agent-id/forge (demoMinted)", Type: "demoMinted"},
				}
			},
			wantCode: steelthread.CodeCredentialNotMintable,
			wantSev:  steelthread.SeverityHard,
			names:    "demo-agent-id/forge (demoMinted)",
		},
		{
			// Gap 2. The recorded output is post-transform and replay would
			// transform it AGAIN; the capture cannot invert that, so it refuses.
			name: "a tool output a guard rewrote",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Records.TransformedToolUseIDs = map[string]string{"tu_1": "contentguard"}
			},
			wantCode: steelthread.CodeTransformedOutput,
			wantSev:  steelthread.SeverityHard,
			names:    "contentguard",
		},
		{
			name: "live secret material surviving the scrub",
			doctor: func(t *testing.T, in *steelthread.SelfCheckInput) {
				plant(t, in, "03-agent.yaml",
					[]byte("  annotations:\n    debug/last-token: "+liveTokenValue+"\n"))
			},
			wantCode: steelthread.CodeSecretLeak,
			wantSev:  steelthread.SeverityHard,
			names:    "03-agent.yaml",
		},
		{
			// The surface that used to go unscanned, and the reason the scan
			// reads Emitted rather than Files. A credential an upstream tool
			// RETURNED is in no Secret the capture reads and in no manifest —
			// it is in the transcript, which is bundle.json.
			name: "live secret material inside a captured tool result",
			doctor: func(t *testing.T, in *steelthread.SelfCheckInput) {
				plant(t, in, "bundle.json", []byte("\n// "+liveTokenValue+"\n"))
			},
			wantCode: steelthread.CodeSecretLeak,
			wantSev:  steelthread.SeverityHard,
			names:    "bundle.json",
		},
		{
			name: "an allowed decision with no seeding tuple",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Seed.Unseeded = []string{"crm_company:c2#list"}
			},
			wantCode: steelthread.CodeUnseededAllow,
			wantSev:  steelthread.SeverityHard,
			names:    "crm_company:c2#list",
		},
		{
			name: "a truncated trigger body cannot be re-signed",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.TriggerChannel = "demoforge-input"
				in.Records.Trigger = &triggerdelivery.Content{
					Kind: "demoforge", Event: "pull_request", ChannelKey: "demoforge:acme/widgets#7",
					Body: []byte(`{"action":"ope`), Truncated: true,
				}
			},
			wantCode: steelthread.CodeTruncatedTrigger,
			wantSev:  steelthread.SeverityHard,
			names:    "trigger",
		},
		{
			// Every transport a bundle can replay is named by the AgentClass or
			// by the session's resolved status — an MCP server, a sidecar
			// toolbox, a toolBundle. A name matching none of them got no
			// output from the fold and would diverge at replay with nothing
			// pointing back here.
			name: "a tool call resolving to no declared transport at all",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Records.Turns = append(in.Records.Turns,
					assistantCall(4, "tu_3", "run_shell", `{"cmd":"ls"}`))
			},
			wantCode: steelthread.CodeUnroutableToolCall,
			wantSev:  steelthread.SeverityHard,
			names:    "run_shell",
		},
		{
			// toolOutputs is keyed SERVER-side because that is what
			// h.MCP.OnTool registers. Two prefixes exposing the same
			// server-side name collapse into one merged sequence the single
			// fake stub genuinely cannot tell apart.
			name: "two MCP servers exposing the same server-side tool name",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.DeclaredTools["widgetco"] = []string{"list_widgets"}
			},
			wantCode: steelthread.CodeMCPNameCollision,
			wantSev:  steelthread.SeverityHard,
			names:    "list_widgets",
		},
		{
			// Two consecutive assistant turns, or a capture opening on one,
			// yield a zero-valued bt.Expect. checkExpect then asserts NOTHING,
			// which defeats the divergence contract the format rests on.
			name: "a step whose expect block asserts nothing",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Folded.LLM[0].Expect = bt.Expect{}
			},
			wantCode: steelthread.CodeEmptyExpect,
			wantSev:  steelthread.SeverityHard,
			names:    "step 0",
		},
		{
			name: "a triggered session captured before trigger_delivery shipped",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.TriggerChannel = "demoforge-input"
			},
			wantCode: steelthread.CodeNoTriggerRecord,
			wantSev:  steelthread.SeverityWarn,
			names:    "trigger",
		},
		{
			name: "a session that never spoke to the user",
			doctor: func(t *testing.T, in *steelthread.SelfCheckInput) {
				t.Helper()
				require.Len(t, in.Folded.LLM, 2)
				in.Folded.LLM[1].Reply = []bt.ReplyPart{{BareText: "thinking about it"}}
			},
			wantCode: steelthread.CodeNoAgentReply,
			wantSev:  steelthread.SeverityWarn,
			names:    "reply",
		},
		{
			// DeriveApprovalSettings' fourth return: a tool-call approval whose
			// interaction category is not durably recorded. A human must add
			// autoApprove by hand, or the replay hangs waiting for an approver.
			name: "a decided approval whose category cannot be recovered",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.UndeterminedApprovals = []string{"tu_9"}
			},
			wantCode: steelthread.CodeUndeterminedApprovalCategory,
			wantSev:  steelthread.SeverityWarn,
			names:    "tu_9",
		},
		{
			// What toolErrors still cannot express. The map is registered
			// through MCPStub.OnToolError, keyed by tool NAME and winning over
			// every canned result, so one tool cannot error on one call and
			// succeed — or error differently — on another.
			name: "one tool whose calls did not all fail the same way",
			doctor: func(_ *testing.T, in *steelthread.SelfCheckInput) {
				in.Folded.ErrorResults = []steelthread.ErrorResult{
					{TurnIndex: 2, UseID: "tu_1", Tool: "acme_list_widgets", Server: "list_widgets",
						Body: "mcp: upstream returned 503"},
					{TurnIndex: 4, UseID: "tu_5", Tool: "acme_list_widgets", Server: "list_widgets",
						Body: "mcp: upstream returned 429"},
				}
				in.Folded.NonUniformErrorTools = []string{"list_widgets"}
			},
			wantCode: steelthread.CodeToolErrorNotUniform,
			wantSev:  steelthread.SeverityHard,
			names:    "tu_5 at turn 4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := cleanCapture(t)
			tc.doctor(t, &in)

			got := steelthread.SelfCheck(in)
			f := findByCode(t, got, tc.wantCode)
			assert.Equal(t, tc.wantSev, f.Severity)
			assert.Contains(t, f.Message, tc.names,
				"a finding that does not name the offending thing sends the reader back to the logs")
		})
	}
}

// TestSelfCheck_CleanCaptureHasNoFindings is the negative control, and the one
// that stops the checks from firing on everything. Without it a check with an
// inverted condition passes every case above.
func TestSelfCheck_CleanCaptureHasNoFindings(t *testing.T) {
	assert.Empty(t, steelthread.SelfCheck(cleanCapture(t)))
}

// TestSelfCheck_UnseededAllowSaysWhichSlotCaseTheReaderIsIn pins the one thing
// the finding's key cannot say: WHY this permission survived the declared-slot
// exemption.
//
// Same code and same severity in both rows — the consequence is identical, and
// a second code would read as a second defect. What differs is the reader's
// next move. With slots declared, the class has been asked and has answered:
// the grant is pre-existing state, go find the relationship. With none
// consulted, a collected slot binding has NOT been ruled out and the first
// thing to check is whether the class has an AgentSessionGrants at all —
// otherwise a capture taken against an unreconciled class sends someone hunting
// a tuple that was never missing.
func TestSelfCheck_UnseededAllowSaysWhichSlotCaseTheReaderIsIn(t *testing.T) {
	cases := []struct {
		name          string
		declaredSlots int
		wantSubstring string
		notSubstring  string
	}{
		{
			name:          "slots were declared and this pair is not one: point at pre-existing state",
			declaredSlots: 4,
			wantSubstring: "declares 4 authz slot(s) and this (resourceType, permission) pair is not one of them",
			notSubstring:  "has not been ruled out",
		},
		{
			name:          "no slot declaration was consulted: point at the missing AgentSessionGrants",
			declaredSlots: 0,
			wantSubstring: "has not been ruled out",
			notSubstring:  "is not one of them",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := cleanCapture(t)
			in.Seed.Unseeded = []string{"crm_company:c2#list"}
			in.Seed.DeclaredSlots = tc.declaredSlots

			f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeUnseededAllow)
			assert.Equal(t, steelthread.SeverityHard, f.Severity,
				"a permission matching no declared slot is still a grant the fixture cannot reproduce")
			assert.Contains(t, f.Message, "crm_company:c2#list")
			assert.Contains(t, f.Message, tc.wantSubstring)
			assert.NotContains(t, f.Message, tc.notSubstring,
				"one message must not carry the other case's advice")
		})
	}
}

// TestSelfCheck_AnUngatedSidecarToolIsNotRefused is the POSITIVE half of
// sidecar support, and the case the capture used to get wrong.
//
// A sidecar tool is an MCP tool at every layer that matters here:
// sidecartoolbox.Synthesize hands mcptool.Synthesize a synthetic MCPServer, and
// at replay the same single fake MCP stub serves it. So a sidecar call resolves
// to a declared prefix, the fold emits a normal toolOutputs entry keyed
// server-side, and nothing is refused.
//
// Before sidecars were read out of the session's resolved status, every one of
// these fell through to "not an MCP server" and raised a spurious hard
// sandbox-tool-call — which is exactly what refused a real capture.
func TestSelfCheck_AnUngatedSidecarToolIsNotRefused(t *testing.T) {
	in := cleanCapture(t)
	in.DeclaredTools["kube"] = []string{"pods"}
	in.Records.Turns = append(in.Records.Turns, assistantCall(4, "tu_3", "kube_pods", `{}`))
	in.PlaceholderTools = []string{"pods"}

	got := steelthread.SelfCheck(in)
	assert.False(t, steelthread.HasHardFinding(got),
		"an ungated sidecar tool is an ordinary MCP tool and must be capturable: %v", got)
	for _, f := range got {
		assert.NotEqual(t, steelthread.CodeUnroutableToolCall, f.Code,
			"a sidecar tool is served by the same fake MCP stub as a remote one; calling it unroutable "+
				"is the bug that refused a real capture")
	}
}

// TestSelfCheck_ACalledSecretGatedSidecarToolIsNotRefused is the retired
// secret-gated-sidecar check, inverted.
//
// That check refused any transcript calling a tool of a sidecar declaring
// spec.secretInputs, because such a sidecar's tools are synthesized only once a
// PRODUCER publishes the per-session secret-output Secret, and a bundle has no
// producer. RewriteFixture now drops spec.secretInputs from the emitted CR — at
// replay the sidecar IS the fake MCP stub and needs no credential — so the gate
// is not declared and the tools are synthesized like any other's.
//
// Nothing in SelfCheckInput distinguishes a gated sidecar from an ungated one
// any more, which is the point: the self-check reads the sidecar's tools through
// DeclaredTools exactly as it reads a remote MCP server's. This test exists so
// that reinstating a refusal has to break something.
func TestSelfCheck_ACalledSecretGatedSidecarToolIsNotRefused(t *testing.T) {
	in := cleanCapture(t)
	in.DeclaredTools["kube"] = []string{"pods"}
	in.Records.Turns = append(in.Records.Turns, assistantCall(4, "tu_3", "kube_pods", `{}`))
	in.PlaceholderTools = []string{"pods"}

	got := steelthread.SelfCheck(in)
	assert.False(t, steelthread.HasHardFinding(got),
		"a called sidecar tool must be capturable whether or not its toolbox declared a secret input: %v", got)
}

// TestSelfCheck_AToolThatOnlyEverFailedUpstreamIsNotRefused pins that a
// uniform upstream failure passes, which is the whole point of toolErrors
// existing.
//
// It also pins that the placeholder WARNING still fires for such a tool, and
// that is not a wart: the tool has no observed output, so the entry the emitted
// bundle carries for it really was invented, and a reader must not have to
// guess which outputs were observed. The value is never served — OnToolError
// wins — but it has to exist for the tool to appear in the stub's tools/list.
func TestSelfCheck_AToolThatOnlyEverFailedUpstreamIsNotRefused(t *testing.T) {
	in := cleanCapture(t)
	// The declared tool's calls all came back an upstream error: no output
	// collected, a toolErrors entry in its place, and a synthesized placeholder
	// so the tool is still catalogued. The shape Capture produces — see
	// TestCapture_AnUpstreamFailureEmitsAToolError.
	in.Folded.ToolOutputs = map[string]json.RawMessage{"list_widgets": json.RawMessage(`{}`)}
	in.Folded.ToolErrors = map[string]bt.ToolError{"list_widgets": {Message: "mcp: upstream returned 503"}}
	in.Folded.ErrorResults = []steelthread.ErrorResult{{
		TurnIndex: 2, UseID: "tu_1", Tool: "acme_list_widgets", Server: "list_widgets",
		Body: "mcp: upstream returned 503",
	}}
	in.PlaceholderTools = []string{"list_widgets"}

	got := steelthread.SelfCheck(in)
	assert.False(t, steelthread.HasHardFinding(got),
		"a tool that failed the same way on every call is expressible: %v", got)
	assert.Equal(t, steelthread.SeverityWarn,
		findByCode(t, got, steelthread.CodePlaceholderToolOutputs).Severity,
		"the entry was invented, so the reader is told")
}

// TestSelfCheck_IsDeterministic pins that two runs over one capture produce the
// same findings in the same order. A capture is a reproducibility artifact, and
// several checks read maps — a set ranged over directly would reorder between
// runs and make the emitted report's own diff unreadable.
func TestSelfCheck_IsDeterministic(t *testing.T) {
	in := cleanCapture(t)
	in.DeclaredTools["acme"] = append(in.DeclaredTools["acme"], "archive_widget", "purge_widget")
	in.DeclaredTools["widgetco"] = []string{"list_widgets", "archive_widget"}
	in.Records.TransformedToolUseIDs = map[string]string{"tu_1": "contentguard", "tu_2": "toolguard"}

	first := steelthread.SelfCheck(in)
	require.NotEmpty(t, first)
	for range 8 {
		assert.Equal(t, first, steelthread.SelfCheck(in))
	}
}

// TestHasHardFinding_IgnoresWarnings pins that warnings do not fail the
// command. A triggered session captured before the Kind shipped is still worth
// emitting as a userTurns bundle.
func TestHasHardFinding_IgnoresWarnings(t *testing.T) {
	assert.False(t, steelthread.HasHardFinding([]steelthread.Finding{
		{Severity: steelthread.SeverityWarn, Code: steelthread.CodeNoTriggerRecord},
	}))
	assert.True(t, steelthread.HasHardFinding([]steelthread.Finding{
		{Severity: steelthread.SeverityWarn, Code: steelthread.CodeNoTriggerRecord},
		{Severity: steelthread.SeverityHard, Code: steelthread.CodeSecretLeak},
	}))
	assert.False(t, steelthread.HasHardFinding(nil))
}

// TestSelfCheck_EmptySecretValueNeverLeaks pins the one input that would make
// the secret-leak check fire on every file it is handed: an empty Value is a
// substring of everything. A capture that read a Secret key holding no bytes
// must not turn every emitted file into a hard finding.
func TestSelfCheck_EmptySecretValueNeverLeaks(t *testing.T) {
	in := cleanCapture(t)
	in.LiveSecrets = append(in.LiveSecrets, steelthread.LiveSecret{Name: "demo-agent-creds/empty", Value: ""})
	assert.Empty(t, steelthread.SelfCheck(in))
}

// TestSelfCheck_SecretLeakMessageDoesNotEchoTheSecret pins that the finding
// names the file and the secret's NAME and never its value. The findings are
// printed to a terminal and land in whatever captured that terminal, so a check
// that exists to catch leaked credentials must not leak one itself.
func TestSelfCheck_SecretLeakMessageDoesNotEchoTheSecret(t *testing.T) {
	in := cleanCapture(t)
	plant(t, &in, "03-agent.yaml", []byte("\n# "+liveTokenValue+"\n"))

	f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSecretLeak)
	assert.Contains(t, f.Message, "03-agent.yaml")
	assert.Contains(t, f.Message, "demo-agent-creds/bot-token")
	assert.NotContains(t, f.Message, liveTokenValue,
		"the finding must not reprint the credential it caught")
}

// TestSelfCheck_SecretScanRefusesWhenItCannotRun pins the one input whose
// emptiness would otherwise read as a clean bill of health.
//
// Every other check reports what it FOUND, so an empty input honestly means
// "nothing wrong". The leak scan is the exception: with no live values to look
// for it produces no findings while having proven nothing, and its failure mode
// is a credential committed to a repo. The distinction that keeps the refusal
// from being noise is ShadowedSecrets — a placeholder standing in for a NAMED
// live Secret proves credential material existed to be read, per Secret.
func TestSelfCheck_SecretScanRefusesWhenItCannotRun(t *testing.T) {
	t.Run("a shadowed Secret with no live values supplied: hard finding", func(t *testing.T) {
		in := cleanCapture(t)
		in.LiveSecrets = nil

		f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSecretCheckSkipped)
		assert.Equal(t, steelthread.SeverityHard, f.Severity)
		assert.Contains(t, f.Message, "demo-agent-creds",
			"the finding must name the Secret whose value nobody read")
	})

	t.Run("live values supplied but every one of them empty: hard finding", func(t *testing.T) {
		// The populated-but-useless slice. The scan skips every empty Value —
		// "" matches every file — so this scans for nothing and reports clean
		// unless the gate counts what is SCANNABLE rather than what is present.
		// Reachable from a live cluster read: a blank Secret key, an
		// unpopulated Value, a partial failure yielding valid-looking entries.
		in := cleanCapture(t)
		in.LiveSecrets = []steelthread.LiveSecret{
			{Name: "demo-agent-creds/bot-token", Value: ""},
			{Name: "demo-agent-creds/signing-secret", Value: ""},
		}

		f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSecretCheckSkipped)
		assert.Equal(t, steelthread.SeverityHard, f.Severity)
		assert.Contains(t, f.Message, "demo-agent-creds")
		assert.Contains(t, f.Message, "2 LiveSecrets entries",
			"the message must not claim the slice was empty when it was merely useless")
	})

	t.Run("a value read for a DIFFERENT Secret: hard finding naming the unread one", func(t *testing.T) {
		// The case the coarse gate could not see. It asked only "is anything
		// scannable at all?", so one Secret read stood in for every Secret
		// emitted, and a fixture substituting three placeholders passed on the
		// strength of one value.
		in := cleanCapture(t)
		in.ShadowedSecrets = []string{"demo-agent-creds", "demo-mcp-token"}

		f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSecretCheckSkipped)
		assert.Contains(t, f.Message, "demo-mcp-token")
		assert.NotContains(t, f.Message, "demo-agent-creds",
			"the Secret that WAS read must not be reported")
	})

	t.Run("a live Secret that was read and held nothing: no finding", func(t *testing.T) {
		// "Nobody looked" and "somebody looked and there was nothing there" are
		// different answers, and only the first is a failure. A live cluster
		// produces the second routinely: a browser-started session's Channel
		// credentials Secret is created EMPTY, because the browser surface needs
		// no credential, and the fixture still emits a placeholder for it.
		in := cleanCapture(t)
		in.LiveSecrets = []steelthread.LiveSecret{{Name: "demo-agent-creds", EmptyRead: true}}

		for _, f := range steelthread.SelfCheck(in) {
			assert.NotEqual(t, steelthread.CodeSecretCheckSkipped, f.Code,
				"a Secret that was read and held nothing is a scan that RAN: %s", f.Message)
		}
	})

	t.Run("an EmptyRead naming a DIFFERENT Secret: hard finding", func(t *testing.T) {
		// The marker must not be usable as a blanket excuse. It says something
		// about ONE Secret.
		in := cleanCapture(t)
		in.LiveSecrets = []steelthread.LiveSecret{{Name: "some-other-secret", EmptyRead: true}}

		f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSecretCheckSkipped)
		assert.Contains(t, f.Message, "demo-agent-creds")
	})

	t.Run("an INVENTED placeholder with nothing to read: no finding", func(t *testing.T) {
		// The false positive the per-Secret gate exists to remove. An
		// AgentClass whose spec.model is absent — every class whose model and
		// credential come from the settings tiers — makes rewriteClass mint a
		// placeholder under a name it invented. No live Secret bears that name,
		// so there is no value to read, and demanding one refuses a valid
		// capture for failing to supply something that does not exist.
		in := cleanCapture(t)
		in.LiveSecrets = nil
		in.ShadowedSecrets = nil

		for _, f := range steelthread.SelfCheck(in) {
			assert.NotEqual(t, steelthread.CodeSecretCheckSkipped, f.Code,
				"a placeholder standing in for nothing must not be charged for an unread value: %s", f.Message)
		}
	})

	t.Run("a document that does not parse: hard finding", func(t *testing.T) {
		// The question ShadowedSecrets cannot answer. A file nobody can parse
		// might hold anything, so a clean scan of it proves nothing about it.
		in := cleanCapture(t)
		in.LiveSecrets = nil
		in.ShadowedSecrets = nil
		in.Files = []steelthread.FixtureFile{{Name: "99-mystery.yaml", YAML: []byte("- just\n- a\n- list\n")}}

		f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeSecretCheckSkipped)
		assert.Contains(t, f.Message, "99-mystery.yaml",
			"'I could not read the file' must block the skip just as an unread Secret does")
		assert.Contains(t, f.Message, "could not be parsed",
			"the message must say WHY the skip was blocked; claiming an unreadable file 'carries a Secret' "+
				"sends the reader hunting something that is not there")
		assert.NotContains(t, f.Message, "carries a Secret")
		assert.Contains(t, f.Message, "cannot unmarshal",
			"the parse error is the only account of why the file was unreadable and must not be dropped")
	})

	t.Run("an agent with no Secrets at all: no finding", func(t *testing.T) {
		in := cleanCapture(t)
		in.LiveSecrets = nil
		in.ShadowedSecrets = nil
		in.Files = []steelthread.FixtureFile{{Name: "03-agent.yaml", YAML: []byte(
			"apiVersion: spicebox.dev/v1alpha1\nkind: AgentClass\nmetadata:\n  name: demo-agent\n")}}

		assert.Empty(t, steelthread.SelfCheck(in),
			"an agent that emitted no Secret had no credential to leak, and refusing it would be noise")
	})
}

// TestSelfCheck_GuardScanRefusesWhenItCannotRun pins the structural twin of the
// secret-scan refusal.
//
// Records.TransformedToolUseIDs is gathered externally from the guard audits
// and has no in-process producer, so an empty map means either "no guard
// rewrote anything" or "nobody gathered". GuardConfigured separates them. This
// is the worse of the two fail-opens for the replay story: a double-transformed
// output is exactly "looks fine, replays wrong".
func TestSelfCheck_GuardScanRefusesWhenItCannotRun(t *testing.T) {
	t.Run("a guard configured with nothing gathered: hard finding", func(t *testing.T) {
		in := cleanCapture(t)
		in.GuardConfigured = true

		f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeGuardScanSkipped)
		assert.Equal(t, steelthread.SeverityHard, f.Severity)
		assert.Contains(t, f.Message, "TransformedToolUseIDs",
			"the finding must name the field a human has to populate")
	})

	t.Run("a guard configured and the audits gathered: no skipped finding", func(t *testing.T) {
		in := cleanCapture(t)
		in.GuardConfigured = true
		in.Records.TransformedToolUseIDs = map[string]string{"tu_1": "contentguard"}

		for _, f := range steelthread.SelfCheck(in) {
			assert.NotEqual(t, steelthread.CodeGuardScanSkipped, f.Code,
				"a gather that found something has demonstrably run: %s", f.Message)
		}
	})

	t.Run("no guard configured at all: no finding", func(t *testing.T) {
		in := cleanCapture(t)
		in.GuardConfigured = false

		assert.Empty(t, steelthread.SelfCheck(in),
			"a class with no tool guard had nothing that could rewrite output, and refusing it would be noise")
	})
}

// TestSelfCheck_ADeliveredArtifactIsNoLongerAFinding pins that the attachment
// case now passes, and it is here rather than deleted because the finding it
// replaces (attached-lost) was HARD and disqualified a shipped bronze scenario.
//
// The fold no longer drops anything: a respond_to_user carrying more than text
// is emitted as a full tool_use with its arguments verbatim, so there is
// nothing left to warn about. TestFold_ARespondToUserCarryingMoreThanTextKeeps-
// EveryArgument is the positive half; this is the half that would notice the
// check being reintroduced and refusing a capture that is now faithful.
func TestSelfCheck_ADeliveredArtifactIsNoLongerAFinding(t *testing.T) {
	in := cleanCapture(t)
	respond := in.Records.Turns[3].Content[0].ToolUse
	require.Equal(t, "respond_to_user", respond.Name)
	respond.Input = []byte(`{"text":"here it is","attached":["art-1"]}`)

	assert.Empty(t, steelthread.SelfCheck(in),
		"a reply that DELIVERED an artifact round-trips whole; refusing it would refuse a faithful capture")
}

// TestSelfCheck_RespondToUserIsNeverUnroutable pins the one name the
// sandbox-tool-call check must know without being told. respond_to_user
// resolves to no MCP prefix and a caller has no reason to list it among
// MetaTools — the fold gives it dedicated handling — so a check keyed on
// MetaTools alone would flag every capture that ever answered a person.
func TestSelfCheck_RespondToUserIsNeverUnroutable(t *testing.T) {
	in := cleanCapture(t)
	in.MetaTools = nil
	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeUnroutableToolCall, f.Code,
			"respond_to_user must never read as an unroutable call: %s", f.Message)
	}
}

// TestSelfCheck_MetaToolsAreNotSandboxCalls is the other half: a name the
// caller DID list runs for real at replay and needs no canned output.
func TestSelfCheck_MetaToolsAreNotSandboxCalls(t *testing.T) {
	in := cleanCapture(t)
	in.Records.Turns = append(in.Records.Turns, assistantCall(4, "tu_3", "update_plan", `{"name":"main"}`))

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeUnroutableToolCall, f.Code,
			"a declared meta tool must not read as an unroutable call: %s", f.Message)
	}
}

// TestSelfCheck_UnmappedTurnsAreReportedIndividually pins that two holes read
// as two findings naming two indices. Collapsing them into one line would make
// a reader fix the first and re-run to discover the second.
func TestSelfCheck_UnmappedTurnsAreReportedIndividually(t *testing.T) {
	in := cleanCapture(t)
	in.Folded.UnmappedTurns = []int{4, 9}

	var msgs []string
	for _, f := range steelthread.SelfCheck(in) {
		if f.Code == steelthread.CodeUnmappedTurn {
			msgs = append(msgs, f.Message)
		}
	}
	require.Len(t, msgs, 2)
	assert.Contains(t, msgs[0], "4")
	assert.Contains(t, msgs[1], "9")
}

// TestSelfCheck_ADeliveredReplyStillCountsAsAReply is the defect the
// capturability survey surfaced, and it is the exact shape the brief warned
// about for DeriveAssertions: two readers of the folded reply, one of them
// updated for the second shape.
//
// A respond_to_user carrying an artifact folds to bt.ReplyPart.ToolUse, so a
// check reading only ReplyPart.Text sees no reply at all — and tells the reader
// a session that DID answer a person "can assert nothing about what the agent
// said." Wrong in the direction that reads as evidence of a thin capture.
func TestSelfCheck_ADeliveredReplyStillCountsAsAReply(t *testing.T) {
	in := cleanCapture(t)
	in.Folded.LLM = []bt.LLMStep{{Reply: []bt.ReplyPart{{
		ToolUse: &bt.ToolUse{
			Name: "respond_to_user",
			Args: json.RawMessage(`{"text":"here is the chart","attached":["art-1"]}`),
		},
	}}}}

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeNoAgentReply, f.Code,
			"the agent answered a person; the reply just was not in the shorthand shape: %s", f.Message)
	}
}

// TestDeriveAssertions_ReadsTheReplyFromBothShapes is checkReplies' companion,
// on the other reader of the same fact. AgentReplyContains is what a bundle
// claims a person was told, so dropping the delivered replies would leave the
// capture asserting nothing about the one turn a person actually read.
func TestDeriveAssertions_ReadsTheReplyFromBothShapes(t *testing.T) {
	folded := steelthread.Folded{LLM: []bt.LLMStep{
		{Reply: []bt.ReplyPart{{Text: "plain words"}}},
		{Reply: []bt.ReplyPart{{ToolUse: &bt.ToolUse{
			Name: "respond_to_user",
			Args: json.RawMessage(`{"text":"words with a chart","attached":["art-1"]}`),
		}}}},
		// Narration and an ordinary tool call are not replies to a person.
		{Reply: []bt.ReplyPart{
			{BareText: "thinking out loud"},
			{ToolUse: &bt.ToolUse{Name: "acme_list_widgets", Args: json.RawMessage(`{"text":"decoy"}`)}},
		}},
	}}

	got := steelthread.DeriveAssertions(steelthread.Records{}, folded)
	assert.Equal(t, []string{"plain words", "words with a chart"}, got.AgentReplyContains,
		"both reply shapes count, and only those two: a `text` argument on some other tool is not a reply")
}

// TestSelfCheck_AnUnprovenErrorOriginWarnsWithoutRefusing pins the severity as
// much as the finding.
//
// Nothing was canned for the call, so the bundle cannot serve a gate's own
// refusal back to a gate that stopped refusing — the reason the entry is
// withheld. What remains is that one error result went unrepresented, and only
// a human can judge whether that step still proves what they wanted, so it is a
// warning: refusing would disqualify a capture that is faithful and replays.
func TestSelfCheck_AnUnprovenErrorOriginWarnsWithoutRefusing(t *testing.T) {
	in := cleanCapture(t)
	// No ToolErrors entry, deliberately — the shape Fold produces for an
	// unproven origin. TestFold_AnUnprovenErrorOriginCansNothing pins that.
	in.Folded.ErrorResults = []steelthread.ErrorResult{{
		TurnIndex: 2, UseID: "tu_1", Tool: "acme_list_widgets", Server: "list_widgets",
		Body:        `approval flow: no one has standing to approve tool "acme_list_widgets"`,
		DeniedInLog: true,
	}}

	got := steelthread.SelfCheck(in)
	f := findByCode(t, got, steelthread.CodeUnprovenErrorOrigin)
	assert.Equal(t, steelthread.SeverityWarn, f.Severity,
		"the capture is faithful and replays; what it cannot say is who authored one error")
	assert.Contains(t, f.Message, "tu_1", "a finding that does not name the call sends the reader back to the logs")
	assert.False(t, steelthread.HasHardFinding(got))
}

// TestSelfCheck_AnOrdinaryUpstreamErrorRaisesNoOriginWarning is the negative
// control. Nothing denied the call, so its error is the upstream's without
// qualification — warning on it would put a note on every real upstream
// failure and train the reader to skip the one that matters.
func TestSelfCheck_AnOrdinaryUpstreamErrorRaisesNoOriginWarning(t *testing.T) {
	in := cleanCapture(t)
	in.Folded.ToolErrors = map[string]bt.ToolError{"list_widgets": {Message: "mcp: upstream returned 503"}}
	in.Folded.ErrorResults = []steelthread.ErrorResult{{
		TurnIndex: 2, UseID: "tu_1", Tool: "acme_list_widgets", Server: "list_widgets",
		Body: "mcp: upstream returned 503",
	}}

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeUnprovenErrorOrigin, f.Code,
			"no record disputes this error's origin: %s", f.Message)
	}
}
