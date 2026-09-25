package builderbundle_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/builderbundle"
	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
)

// builderClassPrompt returns the inline system prompt of the agent-builder
// AgentClass CR (builderAgentClass is agentui_test.go's loader for the class
// itself; this is the one-line extraction skillbody_test.go's own tests want).
func builderClassPrompt(t *testing.T) string {
	t.Helper()
	return builderAgentClass(t).Spec.SystemPrompt.Inline
}

// skillBody returns the folded spec.body of the local//builder-<phase> Skill.
func skillBody(t *testing.T, phase string) string {
	t.Helper()
	b, err := builderbundle.Bundle()
	require.NoError(t, err)
	crs, err := b.CRs()
	require.NoError(t, err)
	want := "local//builder-" + phase
	for _, cr := range crs {
		if cr.GetKind() != "Skill" {
			continue
		}
		cn, _, _ := unstructured.NestedString(cr.Object, "spec", "canonicalName")
		if cn == want {
			body, _, _ := unstructured.NestedString(cr.Object, "spec", "body")
			return body
		}
	}
	t.Fatalf("no Skill CR for %s", want)
	return ""
}

// distinctiveTools are sidecar tool base names that are unambiguous identifiers
// (they never collide with an English word), so a bare occurrence — one NOT
// preceded by "workshop_" — is a real mis-reference the agent could not call.
var distinctiveTools = []string{
	"test_tool", "read_test_log", "stop_test",
	"request_credential", "request_install", "recommend_capability",
	"export_draft", "load_draft", "validate_spec", "render_summary",
	"probe_mcp", "probe_image", "cli_help", "agents_in_thread",
	"project_agent", "test_link", "watch_test", "test_sessions",
	"close_others",
}

// collapseSpace folds every run of whitespace to one space, so an assertion
// about a SENTENCE is not an assertion about where the markdown happened to
// wrap it. The bodies are hand-wrapped prose; a re-wrap is not a change to what
// they say.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// assertNoBareToolNames fails if any distinctive tool token appears without the
// workshop_ prefix.
func assertNoBareToolNames(t *testing.T, phase, body string) {
	t.Helper()
	for _, tool := range distinctiveTools {
		// match the token NOT immediately preceded by "workshop_"
		re := regexp.MustCompile(`(?:^|[^_a-zA-Z])` + regexp.QuoteMeta(tool) + `\b`)
		for _, m := range re.FindAllStringIndex(body, -1) {
			// the char before the token start (skip if it's the "workshop_" underscore case,
			// which the regex already excludes via the [^_...] class — but double-check the
			// literal prefix isn't present)
			idx := m[1] - len(tool)
			if idx >= len("workshop_") && body[idx-len("workshop_"):idx] == "workshop_" {
				continue
			}
			t.Errorf("%s skill: bare tool name %q (must be workshop_%s)", phase, tool, tool)
		}
	}
}

func TestCoreLoopSkillBodies(t *testing.T) {
	cases := []struct {
		phase       string
		mustMention []string // prefixed tool names the §3.3 row requires
	}{
		{"assess", []string{"workshop_inventory", "workshop_close_others"}},
		{"tools", []string{"workshop_apply", "workshop_test_tool", "workshop_validate_spec", "workshop_inventory"}},
		{"permissions", nil}, // interview-driven; no required tool call
		{"agent", []string{"workshop_apply"}},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			body := skillBody(t, tc.phase)
			assert.NotContains(t, body, "Filled by plan 6b", "body must be written, not the stub")
			assert.Greater(t, len(body), 400, "a real procedure, not a placeholder")
			for _, tool := range tc.mustMention {
				assert.Contains(t, body, tool, "%s must reference %s", tc.phase, tool)
			}
			assertNoBareToolNames(t, tc.phase, body)
		})
	}
}

// TestToolsSkillNamesTheAdapterTier guards plan 8a's Task 3: before this
// task, tier 3 (the adapter we ship) named no image, tool, or field — a
// builder reading it had nothing to act on. The image name is DERIVED from
// apimage.APIAdapter.Name rather than transcribed, so a future rename of the
// adapter image (a one-line change in apimage) is caught here instead of
// silently leaving the skill pointing at a name workshop_inventory no longer
// reports.
func TestToolsSkillNamesTheAdapterTier(t *testing.T) {
	body := skillBody(t, "tools")
	assert.Contains(t, body, "workshop_inventory", "tools must send the builder to inventory for the adapter's image reference")
	assert.Contains(t, body, "workshop_validate_spec", "tools must validate the adapter config before workshop_apply")
	assert.Contains(t, body, apimage.APIAdapter.Name, "tools must name the actual adapter image inventory reports, not a placeholder")
}

// fencedBlockRe finds a plain, unlabeled fenced code block (deliberately not
// "```yaml" — the fence marker itself must carry none of
// TestSkillBodiesAvoidPlatformVocabulary's banned tokens, and the block is
// data the builder authors, not a platform structure). (?s) makes '.' match
// the newlines inside the block.
var fencedBlockRe = regexp.MustCompile("(?s)```\n(.*?)\n[ \t]*```")

// exampleOperationRe pulls the operation name(s) the tier-3 prose calls out
// by name — "one operation, `list_events`" — straight from the prose,
// rather than a second hand-transcribed list in this test that could drift
// from the fenced block below it if either is edited alone.
var exampleOperationRe = regexp.MustCompile("operation,\\s+`([a-zA-Z0-9_-]+)`")

// dedentBlock strips every line's common leading whitespace so a fenced
// block extracted from inside a numbered list item (each line carrying the
// list's own continuation indent) becomes a top-level YAML document
// regardless of how deeply the skill body nests it.
func dedentBlock(s string) string {
	lines := strings.Split(s, "\n")
	min := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " "))
		if min == -1 || n < min {
			min = n
		}
	}
	if min <= 0 {
		return s
	}
	for i, ln := range lines {
		if len(ln) >= min {
			lines[i] = ln[min:]
		} else {
			lines[i] = strings.TrimLeft(ln, " ")
		}
	}
	return strings.Join(lines, "\n")
}

// TestToolsSkillAdapterExampleParses is MAJOR-1(b): the tier-3 worked
// example claimed twice now (commit 55087d415, and the design spec before
// it) to survive validate_spec, and both times the claim was made by
// reading the prose rather than running the real parser — the second time
// caught only by the closing review. This test replaces the claim with a
// fact: it extracts the example's own fenced configuration block and feeds
// it to the SAME apiadapter.Parse the workshop webhook and the CLI's
// SidecarToolbox ValidateFile both run, so a future edit that reintroduces
// an unparseable example (a param with no type, an unrequired path param,
// an auth block missing a mandatory field) fails here instead of shipping
// unnoticed a third time.
func TestToolsSkillAdapterExampleParses(t *testing.T) {
	body := skillBody(t, "tools")

	m := fencedBlockRe.FindStringSubmatch(body)
	require.Len(t, m, 2, "tools skill body must contain one fenced adapter configuration block")
	block := dedentBlock(m[1])

	cfg, err := apiadapter.Parse([]byte(block))
	require.NoError(t, err, "the tools skill's worked adapter example must parse as a real apiadapter.Config:\n%s", block)

	// Pin the "no more and no fewer" rule the tier's own prose teaches: the
	// config's operations must be exactly the ones the prose names by name,
	// not a superset or subset of them.
	matches := exampleOperationRe.FindAllStringSubmatch(body, -1)
	require.NotEmpty(t, matches, `tools skill body must name its example operation(s) as "operation, `+"`"+`name`+"`"+`"`)
	wantNames := make([]string, 0, len(matches))
	for _, mm := range matches {
		wantNames = append(wantNames, mm[1])
	}
	assert.ElementsMatch(t, wantNames, cfg.OperationNames(),
		"the fenced config's operations must be exactly the ones the prose names — no more, no fewer")
}

// TestSkillBodiesAvoidPlatformVocabulary guards §3.2's hard ban on exposing
// platform vocabulary to the person the builder is talking to. It runs over
// all eight phases, every one of which is now a real, written procedure —
// including plan 9a's reproduce body. reproduce is the phase whose own
// subject matter most tempts "namespace" — the lookup it drives exists to
// describe a conversation boundary — so this is the test that would catch a
// future edit that reached for the banned word instead of describing that
// boundary in the person's own terms.
func TestSkillBodiesAvoidPlatformVocabulary(t *testing.T) {
	// §3.2 hard-bans exposing platform vocabulary to the person. The list is
	// the ban; "slot" is not on it, a historical exemption from when the page
	// was authored as slots. "Hook" is the current word, and the bodies name
	// the hooks they repaint (TestSkillBodiesNameTheirHook) — both are
	// model-facing, and neither is banned.
	banned := []string{`\bYAML\b`, `\bCRs?\b`, `\bnamespace\b`, `\bSpiceDB\b`, `\bkubectl\b`}
	for _, phase := range []string{"assess", "tools", "permissions", "agent", "test", "deliver", "handoff", "reproduce"} {
		body := skillBody(t, phase)
		for _, pat := range banned {
			assert.NotRegexp(t, pat, body, "%s skill body must not expose platform vocabulary %q", phase, pat)
		}
	}
}

// TestSkillBodiesReferenceTheirTools pins each body to the tools its procedure
// actually depends on, so a future edit cannot quietly reduce one back to
// unverifiable prose. Named for what it checks rather than for the finish
// phases it started with: reproduce is an ENTRY phase and belongs in the same
// table, which only works while the name stays true of every row.
func TestSkillBodiesReferenceTheirTools(t *testing.T) {
	cases := []struct {
		phase       string
		mustMention []string
	}{
		{"reproduce", []string{"workshop_agents_in_thread", "read_thread_history", "workshop_project_agent"}},
		{"agent", []string{"workshop_project_agent"}},
		{"test", []string{"workshop_test_link", "workshop_watch_test", "workshop_test_sessions", "workshop_read_test_log", "workshop_stop_test", "workshop_project_agent"}},
		{"deliver", []string{"workshop_export_draft", "artifact_await", "respond_to_user", "workshop_request_install", "workshop_close_others"}},
		{"handoff", []string{"workshop_recommend_capability", "workshop_export_draft", "agent_work_complete", "workshop_close_others"}},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			body := skillBody(t, tc.phase)
			assert.NotContains(t, body, "Filled by plan 6b", "body must be written, not the stub")
			assert.Greater(t, len(body), 400, "a real procedure, not a placeholder")
			for _, tool := range tc.mustMention {
				assert.Contains(t, body, tool, "%s must reference %s", tc.phase, tool)
			}
			assertNoBareToolNames(t, tc.phase, body)
		})
	}
}

// TestSkillsCloseOtherWorkshopsOnlyWhenAsked pins task 5 of the workshop-limit
// series: assess, deliver, and handoff each tell the builder how to release
// the person's other workshops against the per-person start-gate limit — but
// only on the person's own request, never on the builder's initiative and
// never as a workaround for a refusal, and never the workshop the builder is
// currently running in. The two phrases are pinned verbatim (not a
// paraphrase), compared whitespace-normalized as TestReproduceSkillDropsThe9aProhibition
// explains: SKILL.md is hard-wrapped, so a phrase that straddles a line break
// never matches raw.
func TestSkillsCloseOtherWorkshopsOnlyWhenAsked(t *testing.T) {
	for _, phase := range []string{"assess", "deliver", "handoff"} {
		t.Run(phase, func(t *testing.T) {
			body := skillBody(t, phase)
			normalized := strings.Join(strings.Fields(body), " ")
			assert.Contains(t, normalized, "workshop_close_others", "%s must name the tool that closes other workshops", phase)
			assert.Contains(t, normalized, "never the one you are in", "%s must say the tool never closes the workshop it is running in", phase)
			assert.Contains(t, normalized, "only when they ask", "%s must say this happens only on the person's own request", phase)
			assertNoBareToolNames(t, phase, body)
		})
	}
}

// TestReproduceSkillDropsThe9aProhibition guards plan 9b's Task 3: 9a shipped
// builder-reproduce/SKILL.md with a deliberate prohibition — the new agent
// must be described "never as one that calls on the agents you just found" —
// correct only because the platform could not yet express a rehearsed
// hand-off. 9b adds workshop_project_agent, which makes that prohibition
// false; leaving it in place would ship a skill instructing the builder never
// to do the very thing this plan built. Keyed on "calls on the agents you
// just found" rather than the paragraph's other sentence ("does not hand work
// back to the agents that did it the first time") because it is the more
// self-contained false claim: the surviving text is expected to talk about
// hand-offs and agents together throughout — that's the whole feature — so a
// phrase built only from those common words would false-positive on the
// replacement prose itself. This one recurs only if the actual prohibition is
// pasted back in.
//
// The body is compared after collapsing all whitespace runs to a single
// space (strings.Fields + Join), not raw: SKILL.md is hard-wrapped at ~80
// columns and the fold into spec.body keeps that literal line break, so the
// source text actually reads "...you just\nfound" — a bare
// strings.Contains against the space-joined phrase silently never matches
// (a false pass) regardless of which sentence is present. Proven by
// deliberately restoring 9a's original paragraph verbatim and re-running
// this test: it failed to catch the regression until this normalization was
// added, then caught it correctly.
func TestReproduceSkillDropsThe9aProhibition(t *testing.T) {
	body := skillBody(t, "reproduce")
	normalized := strings.Join(strings.Fields(body), " ")
	assert.NotContains(t, normalized, "calls on the agents you just found",
		"reproduce must no longer prohibit handing off to an agent that took part in this conversation — 9b makes that false")
}

// The four-rule necessity test (§3.4) must be present in handoff, or the builder
// would escalate a mere tool gap to a platform handoff.
func TestHandoffCarriesFourRuleTest(t *testing.T) {
	body := strings.ToLower(skillBody(t, "handoff"))
	// the four distinguishing ideas of §3.4
	assert.Contains(t, body, "reduced agent", "rule 2: propose the reduced agent")
	assert.True(t,
		strings.Contains(body, "platform gap") || strings.Contains(body, "platform-level"),
		"rule 4: a platform gap, not a tool gap")
	assert.Contains(t, body, "tool gap", "rule 4: contrast with a tool gap")
	// the gate is explicit that ALL conditions must hold
	assert.True(t,
		strings.Contains(body, "all four") || strings.Contains(body, "only when") || strings.Contains(body, "all of"),
		"the gate requires ALL rules to hold")
}

// TestSkillBodiesNameTheirHook pins each phase skill to the hook(s) it
// repaints (spec §11: "the 6b skills gain one instruction each: which hook to
// repaint at their phase") and to the timeline every phase advances. A body
// that says only "call update_view" leaves the builder to guess the region;
// the hook names here are the page's (agentui_test.go pins them in order).
func TestSkillBodiesNameTheirHook(t *testing.T) {
	cases := []struct {
		phase string
		hooks []string
	}{
		{"assess", []string{"agent", "questions", "phase"}},
		{"tools", []string{"tools", "phase"}},
		{"permissions", []string{"permissions", "phase"}},
		{"agent", []string{"agent", "phase"}},
		{"test", []string{"testRun", "phase"}},
		{"deliver", []string{"deliver", "questions", "phase"}},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			body := skillBody(t, tc.phase)
			for _, h := range tc.hooks {
				assert.Contains(t, body, "`"+h+"`", "%s must name the `%s` hook it repaints", tc.phase, h)
			}
		})
	}
}

// TestSkillBodiesAskThroughThePage pins the question rule the person set: a
// question goes on the page as a question card in the `questions` hook, and
// the conversation is the fallback only when the page cannot carry it.
func TestSkillBodiesAskThroughThePage(t *testing.T) {
	for _, phase := range []string{"assess", "permissions"} {
		t.Run(phase, func(t *testing.T) {
			body := skillBody(t, phase)
			assert.Contains(t, body, "`questions` hook", "%s asks through the page", phase)
			assert.Contains(t, body, "ap:question", "%s names the question card", phase)
			assert.Contains(t, body, "clear it", "%s clears an answered question", phase)
		})
	}
}

// TestDeliverSkillAttachesTheDraft pins the delivery of the exported draft:
// awaited as an artifact, attached to the reply, and shown on the page.
func TestDeliverSkillAttachesTheDraft(t *testing.T) {
	body := strings.Join(strings.Fields(skillBody(t, "deliver")), " ")
	for _, want := range []string{"`handle`", "`artifact_await`", "`attached`", "`ap:attachment`", "`artifactId`"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "link or handle it returns", "the old prose that handed the person a store reference")
}

// TestDeliverSkillStage pins Deliver's shift to a painted stage: the outcome
// on the page (a heading, the exported draft, three plain choices) rather
// than narrated in prose, and the two new choices each with their own
// section.
func TestDeliverSkillStage(t *testing.T) {
	body := skillBody(t, "deliver")
	for _, s := range []string{"is ready", "Install for real", "Hand the draft to someone", "Test it again", "Nothing runs until then"} {
		assert.Contains(t, body, s)
	}
}

// TestSkillsUseTheNoticeAndNeverClearTheAgentHook pins the moment the reply
// modal used to fire on: the test skill paints an ap:notice carrying the
// test_done/thats_not_right buttons, instead of telling the person in prose
// and parking. Task 4 replaced the notice's own timing prose with a state
// table the `testRun` hook's ap:status line drives (never a question in
// prose about whether it is running), and the table itself says the notice
// comes and goes with the state (present while running/paused, gone once
// unwatched or ended — "no notice, no question"), so those are the
// invariants pinned here now rather than the old narrated wording. And the
// assess skill never clears the `agent` hook: the hook's own intent says
// so, because it holds the person's words and the settled facts built on
// them, and the page stages it by its step rather than the skill clearing
// it.
func TestSkillsUseTheNoticeAndNeverClearTheAgentHook(t *testing.T) {
	test := skillBody(t, "test")
	for _, want := range []string{"`ap:notice`", "`test_done`", "`thats_not_right`", "never a question in prose", "no notice, no question"} {
		assert.Contains(t, test, want)
	}
	assess := strings.Join(strings.Fields(skillBody(t, "assess")), " ")
	assert.NotContains(t, assess, "clear the `agent` hook", "the agent hook is never cleared; the skill must not say to clear it")
	assert.Contains(t, assess, "`agent`", "the skill still names the hook it repaints")
}

// TestSkillBodiesRunPhasesBackToBack pins the pause discipline a live session
// broke: the builder finished Assess, said "now I'll move on to tools", and
// called agent_work_complete — a pause the person could only read as the
// session dying. Every phase that has a next phase must say to load it and
// keep going in the same turn, never to pause; only deliver may end the
// session, and it must say so by name.
func TestSkillBodiesRunPhasesBackToBack(t *testing.T) {
	for _, phase := range []string{"assess", "tools", "permissions", "agent", "test"} {
		t.Run(phase, func(t *testing.T) {
			body := strings.Join(strings.Fields(skillBody(t, phase)), " ")
			assert.Contains(t, body, "in this same turn", "%s must continue into the next phase within the turn", phase)
			assert.Contains(t, body, "do not pause", "%s must forbid pausing between phases", phase)
			assert.Contains(t, body, "`agent_work_complete`", "%s must name the call it is forbidding", phase)
		})
	}
	deliver := strings.Join(strings.Fields(skillBody(t, "deliver")), " ")
	assert.Contains(t, deliver, "`agent_work_complete`", "deliver is the one phase that ends the session, and says so")
	assert.Contains(t, deliver, "only now", "deliver says this is the only time to end")
}

// Whose access the new agent uses is decided by the agent's identity mode,
// never by anything on a tool — and no skill said so. A live builder given
// "each person with their own GitHub and Linear access" spent ~30 turns
// guessing field names to attach an identity to an MCPServer, one
// approval-gated apply per guess, until the toolguard breaker tripped
// (oap-desktop, 2026-09-13). Tools must name the mapping before any tool is
// authored, and Agent must carry it into the composed agent. Compared on the
// whitespace-normalized body, as TestReproduceSkillDropsThe9aProhibition
// explains: SKILL.md is hard-wrapped, and a phrase that straddles a line
// break never matches raw.
func TestToolsAndAgentSkillsNameWhoseAccessTheAgentUses(t *testing.T) {
	tools := strings.Join(strings.Fields(skillBody(t, "tools")), " ")
	assert.Contains(t, tools, "`identityMode: userPassthrough`", "tools: per-person access is the agent's identity mode")
	assert.Contains(t, tools, "Nothing on a tool binds it to an identity", "tools: stop the field-name hunt on the tool")
	assert.Contains(t, tools, "do not author an AgentIdentity for it", "tools: per-person access needs no authored identity")
	agent := strings.Join(strings.Fields(skillBody(t, "agent")), " ")
	assert.Contains(t, agent, "`identityMode: userPassthrough`", "agent: the composed agent carries the mode Tools settled")
	assert.Contains(t, agent, "never on a tool", "agent: the identity attaches on the agent alone")
}

// Setting the mode is not enough on its own: a server whose auth block names
// neither `credential` nor `provider` contributes NO requirement, so the
// per-person identity is written with nothing in it and every call goes out
// unauthenticated — an agent nobody can ever connect an account to. The first
// live person-side test shipped exactly that (oap-desktop, 2026-09-13): two
// servers carrying `auth: {type: oauth}` and nothing else. Tools must say
// which fields carry the account slot, by name, where it teaches the mode.
func TestToolsSkillNamesTheAuthFieldsThatLinkAnAccount(t *testing.T) {
	tools := strings.Join(strings.Fields(skillBody(t, "tools")), " ")
	assert.Contains(t, tools, "`auth.credential`", "tools: names the field each person's linked account is stored under")
	assert.Contains(t, tools, "`auth.provider`", "tools: names the field that picks the sign-in for an oauth server")
	assert.Contains(t, tools, "unauthenticated", "tools: says what happens without them, so the rule survives a rewrite")
}

// An AgentClass with no budget wedges its own status writes on a cluster
// whose settings tiers declare none: the CRD floors refuse the all-zero
// effective budget, so the Valid condition never moves. The builder class
// carries its own budget for exactly this reason; the fourth live session
// composed an agent without one and re-applied it four times, one approval
// each, against a condition that could not change (oap-desktop, 2026-09-13).
// Until the platform decides what an undeclared budget means, Agent must
// always declare one.
func TestAgentSkillAlwaysDeclaresABudget(t *testing.T) {
	agent := strings.Join(strings.Fields(skillBody(t, "agent")), " ")
	assert.Contains(t, agent, "**A budget, always.**", "agent: a budget is a required composition item")
	assert.Contains(t, agent, "`budget.maxTurns`", "agent: names the fields, so the builder does not guess them")
	assert.Contains(t, agent, "cannot report itself valid", "agent: says why, so the rule survives a rewrite")
}

// The AgentClass refuses to report itself valid while any referenced tool
// lacks permission.stateImpact (ReasonToolPermissionMissing), and the check
// is the class's, so validate_spec's MCPServer dry run cannot surface it.
// Both live runs that reached Agent applied the servers, applied the class,
// read the refusal, and re-applied all three — six approvals for a rule the
// tools skill can state at authoring time (oap-desktop, 2026-09-13).
func TestToolsSkillDeclaresStateImpactUpFront(t *testing.T) {
	tools := strings.Join(strings.Fields(skillBody(t, "tools")), " ")
	assert.Contains(t, tools, "`permission.stateImpact`", "tools: names the field on every tool entry")
	assert.Contains(t, tools, "refuse to report itself valid", "tools: says what happens without it")
}

// TestClassPromptForbidsCredentialAsks pins the class-level rule that
// replaces the removed connect_tool control: the builder never asks the
// person for a credential itself, on the page or in the conversation — the
// platform's own accounts page, or workshop_request_credential's secure
// card, is the only route.
func TestClassPromptForbidsCredentialAsks(t *testing.T) {
	prompt := strings.Join(strings.Fields(builderClassPrompt(t)), " ")
	assert.Contains(t, prompt, "Never ask the person for a key, token, password, or account")
}

// TestTestSkillDrivesThePersonsOwnTest pins the Test phase's shift from a
// builder-narrated rehearsal to the person's own conversation with the
// finished agent: the skill hands over an ap:agentlink (from
// workshop_test_link), asks the Workshop controller to watch
// (workshop_watch_test), and only reads the log (workshop_test_sessions +
// workshop_read_test_log) once told the test moved or the person is done —
// workshop_stop_test remains the one way to end it early, and the retired
// workshop_run_test must never come back.
func TestTestSkillDrivesThePersonsOwnTest(t *testing.T) {
	body := strings.Join(strings.Fields(skillBody(t, "test")), " ")
	for _, want := range []string{"workshop_test_link", "workshop_watch_test", "workshop_test_sessions", "workshop_read_test_log", "workshop_stop_test", "await_user_message"} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "workshop_run_test")
}

// TestTestSkillQuotesTheWatchLinesVerbatim proves the test skill tells the
// builder EXACTLY what workshop_watch_test's delivery will say, not a
// paraphrase — by importing the same six constants the Workshop controller
// delivers (pkg/controllers/workshop/testwatch.go), so a wording change on
// either side shows up here instead of the two silently drifting apart.
// Whitespace-normalized, as TestReproduceSkillDropsThe9aProhibition
// explains: SKILL.md is hard-wrapped, so a quoted line that straddles a line
// break never matches raw.
func TestTestSkillQuotesTheWatchLinesVerbatim(t *testing.T) {
	body := strings.Join(strings.Fields(skillBody(t, "test")), " ")
	for _, want := range []string{
		workshop.TestLineStarted,
		workshop.TestLinePaused,
		workshop.TestLineResumed,
		workshop.TestLineEnded,
		workshop.TestLineStopped,
		workshop.TestLineTimedOut,
	} {
		assert.Contains(t, body, want, "test skill body must quote %q verbatim", want)
	}
}

// TestToolsSkillNeverPaintsAConnectControl pins that connect_tool's removal
// is reflected in the tools skill's own account-connection guidance, not
// just the page: no procedure may tell the builder to paint a row action
// that connects an account, and the credential-ask ban must be stated here
// too, in the phase that used to reach for workshop_request_credential via
// a row action.
func TestToolsSkillNeverPaintsAConnectControl(t *testing.T) {
	body := strings.ToLower(strings.Join(strings.Fields(skillBody(t, "tools")), " "))
	assert.NotContains(t, body, "connect row action")
	assert.Contains(t, body, "never ask the person for a key, token, password, or account")
}

// TestToolsSkillRequiresMCPServerBeforeOAuthMCPCredential pins a live-run
// correction: a builder authored an AgentIdentity and called
// workshop_request_credential(authKind: "oauth-mcp") for two shared accounts
// without ever applying the backing MCPServer, because the body only said to
// "author an AgentIdentity ... call workshop_request_credential" for a shared
// account and never mentioned the MCPServer at all. The person's Connect
// cards were sent and delivered, but 400'd ("No matching service") the
// instant they were clicked — identityd's agent-owned OAuth handler resolves
// the MCPServer backing the credential at click time, and none existed. The
// body must say, for oauth-mcp specifically, that the MCPServer has to be
// applied FIRST — before the request_credential call, not after.
func TestToolsSkillRequiresMCPServerBeforeOAuthMCPCredential(t *testing.T) {
	body := strings.Join(strings.Fields(skillBody(t, "tools")), " ")
	assert.Contains(t, body, "oauth-mcp", "tools: must call out the oauth-mcp authKind specifically")
	assert.Contains(t, body, "`workshop_apply` the MCPServer FIRST",
		"tools: must say the MCPServer is applied before request_credential, not after")
	assert.Contains(t, body, "before you call `workshop_request_credential`",
		"tools: must state the ordering explicitly, not leave it implied")
	assert.Contains(t, body, "No matching service",
		"tools: must name the actual failure so the rule survives a rewrite")
}

// TestToolsSkillNamesProvidersThatCannotDoOAuthMCP pins a second live-run
// correction on the same session: with the MCPServer-first rule in place, the
// builder correctly applied real MCPServers for Slack and GitHub and
// requested oauth-mcp credentials for both — but neither provider supports
// what oauth-mcp needs. Slack has no self-registration endpoint at all
// ("does not advertise registration_endpoint"), and GitHub publishes no
// discoverable OAuth metadata document at any of the addresses discovery
// tried. Both connect attempts failed with the person never told why, and
// the builder had no way to know oauth-mcp could not work here at all. The
// body must name both services and say what actually works for them: a
// pasted token, `pat`/`static`.
func TestToolsSkillNamesProvidersThatCannotDoOAuthMCP(t *testing.T) {
	body := strings.Join(strings.Fields(skillBody(t, "tools")), " ")
	assert.Contains(t, body, "Slack",
		"tools: must name Slack as a provider oauth-mcp cannot connect")
	assert.Contains(t, body, "GitHub",
		"tools: must name GitHub as a provider oauth-mcp cannot connect")
	assert.Contains(t, body, "self-registration endpoint",
		"tools: must say why Slack specifically fails (no self-registration)")
	assert.Contains(t, body, "switch that credential's `authKind` to `pat`/`static`",
		"tools: must tell the builder the concrete recovery action, not just the diagnosis")
}

// TestTestSkillTeachesThatAnEmptyLogIsNotProof pins the correction a live run
// forced: a test run was refused before it could call anything, the builder
// read an empty log, reported "the failure happened before any calls" and
// ended the run — destroying the only record of a failure the same tool result
// was carrying the whole time. The body must send the builder to the reason,
// and must forbid ending a run whose reason it has not read.
func TestTestSkillTeachesThatAnEmptyLogIsNotProof(t *testing.T) {
	body := skillBody(t, "test")
	for _, field := range []string{"failureReason", "failureMessage", "auditEntries"} {
		assert.Contains(t, body, field,
			"the test skill must name the field carrying a refused run's own reason")
	}
	collapsed := strings.Join(strings.Fields(body), " ")
	assert.Contains(t, collapsed, "empty log is never proof",
		"the body must say outright that an empty log proves nothing")
	assert.Contains(t, collapsed, "whose reason you have not read",
		"and must forbid ending a run before its reason has been read")
}

// TestTestSkillStateTable pins the state lines the Test phase paints — the
// page, not the conversation, answers "is it running?" — plus the three
// things a row cannot be right without. The moment lives in `since`, never in
// the line's own text: the builder has no clock, so the only time it can
// state is the one the watcher's line handed it, and only the browser knows
// which zone to draw it in. The ap:chat's sessionRef is the `ref`
// workshop_test_sessions returns, not the `name` beside it: a bare name
// cannot address a session, and `ref` is how the row says so in the only
// vocabulary a skill body may use (TestSkillBodiesAvoidPlatformVocabulary
// bans the word the two-field form would need). And the ended row's count
// comes from the log, NEVER from turnCount, which counts model turns of the
// current runner pod and resets when that pod is replaced — and a paused test
// is exactly a pod exit.
//
// The paused row says "Waiting on you since", never "Paused at". The runner
// reports an idle-timeout exit with the same condition reason as parking on
// an await (pkg/agent/runner/status.go, WriteIdle), so an agent that simply
// finished its reply is indistinguishable from one the person walked away
// from — and live, a card read "Paused at" over a chat that was plainly
// waiting for the next message. Both mean the same thing to the person: the
// agent is done and the next message wakes it.
//
// The start control's label and the timeline's Test summary are pinned here
// too, because live the builder chose its own label ("Try it") — which the
// ended row's "never paint a second Start the test" then cannot refer to —
// and left the Test step's summary at "starting now" through running, paused
// and ended.
func TestTestSkillStateTable(t *testing.T) {
	body := skillBody(t, "test")
	for _, line := range []string{
		"Not started",
		"Running since",
		"Still running; I stopped watching at",
		"Waiting on you since",
		"`since`",
		"Ended",
		"ap:status",
		"ap:chat",
		"`embed`",
		"never a question in prose",
		"workshop_read_test_log",
	} {
		assert.Contains(t, body, line)
	}
	collapsed := strings.Join(strings.Fields(body), " ")
	assert.Contains(t, collapsed, "`sessionRef` is that result's `ref`",
		"the chat must be addressed by the whole reference, not the name beside it")
	assert.Contains(t, collapsed, "the number of messages the person sent",
		"the ended row must say what N counts, so it is not read off turnCount")
	assert.Contains(t, collapsed, "labelled exactly **Start the test**",
		"the link's label is fixed, or the rest of the skill names a control that is not on the page")
	assert.Contains(t, collapsed, "Test step's summary",
		"every row must repaint the timeline too, or the rail says 'starting now' through a finished test")
	assert.NotContains(t, body, "Paused at",
		"an idle test is indistinguishable from a finished reply, and both are the agent waiting on the person")
	assert.NotContains(t, body, "turnCount",
		"turnCount is the current pod's model turns and resets on pod replacement; the skill must not reach for it")
	assert.NotContains(t, body, "opens a conversation with their new agent in a new tab")
}

// TestDeliverSkillAsksOnceAndRepaintsThePhaseOnTheWayBack pins two Deliver
// corrections. The stage's own three buttons ARE the offer to install, so an
// ap:question painted beside them asks the same thing twice; the question
// card is the fallback for a prose reply that chose none of them. And going
// back to test again must repaint the `phase` hook with Test active —
// `testRun` is bound to the test step and is off the stage until then, so a
// builder that only loads the test skill leaves the person looking at an
// unchanged page.
func TestDeliverSkillAsksOnceAndRepaintsThePhaseOnTheWayBack(t *testing.T) {
	body := strings.Join(strings.Fields(skillBody(t, "deliver")), " ")
	assert.Contains(t, body, "three buttons you just painted are the offer",
		"deliver must say the buttons are the ask, not paint a second one")
	assert.Contains(t, body, "replies in prose without choosing one",
		"the question card is the fallback, and the body must say when")
	assert.Contains(t, body, "repaint the `phase` hook with Test active",
		"going back to Test must move the timeline, or the person sees nothing")
}

// TestSkillsPlanWithTheDraftsPermissions guards Task 3 of the
// workshop-draft-slot series: after Tasks 1-2 the runner's permission surface
// carries perm:change/remove/test/project:workshop_draft, bindable by naming
// them on a plan phase's own `permissions[]` (spec §5) — but naming them is
// the builder's own choice, and nothing told it which handle covers which
// phase's work. This pins the exact handle string each phase's procedure
// must carry, in the section that starts that phase's plan-worthy work, so a
// future edit cannot quietly drop the one line that makes a phase's approval
// actually cover its calls. Handles are pinned literally (backtick-quoted,
// matching how the bodies already quote every other field/value they teach)
// because the plan tool's `permissions[].handle` only matches the surface's
// exact spelling — a paraphrase would compile as prose and fail as a plan.
func TestSkillsPlanWithTheDraftsPermissions(t *testing.T) {
	cases := []struct {
		phase   string
		handles []string
	}{
		// tools: workshop_apply is the phase's own work; workshop_delete only
		// when an already-applied definition is being replaced.
		{"tools", []string{"perm:change:workshop_draft", "perm:remove:workshop_draft"}},
		// agent: workshop_apply composes the agent; workshop_project_agent (the
		// roster's practice double) is a second, separate call the same phase
		// may also make.
		{"agent", []string{"perm:change:workshop_draft", "perm:project:workshop_draft"}},
		// test: workshop_watch_test is this phase's own work; perm:change
		// recurs because a fix sends the builder back to reapply elsewhere.
		{"test", []string{"perm:test:workshop_draft", "perm:change:workshop_draft"}},
		// handoff: names no tool call of its own toward the slot, but its own
		// rule 4 (platform gap, not a tool gap) must correctly attribute the
		// hand-off rehearsal's approval to the phase that actually grants it.
		{"handoff", []string{"perm:project:workshop_draft"}},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			body := skillBody(t, tc.phase)
			for _, handle := range tc.handles {
				assert.Contains(t, body, "`"+handle+"`", "%s must name %s on the phase's own plan", tc.phase, handle)
			}
			// The handle ALONE is refused, which is why this is pinned beside
			// it rather than left to the model. Under the builder class's
			// enforcing gate with requirePlan, a phase whose permissions name
			// a declared slot type and whose `slots` say nothing about it is
			// rejected outright by plangate.MissingSlotDeclarations — the
			// update_plan call fails and no plan freezes at all. A skill that
			// teaches only the handle therefore costs a turn on the FIRST
			// update_plan of every build. The reverse is worse and silent: a
			// phase declaring the slot and naming no handle falls through
			// boundPermissionsFor's declared[0] fallback and binds `change`
			// with nobody having said so.
			assert.Contains(t, collapseSpace(body), slotDeclarationPhrase,
				"%s tells the builder the handle but not the slot line, so its first update_plan is refused", tc.phase)
			assertNoBareToolNames(t, tc.phase, body)
		})
	}
}

// slotDeclarationPhrase is the one wording every draft-touching skill uses for
// the plan input beside the handle. Pinned as a shared constant, not per
// phase: four paraphrases of one plan-tool rule is how three of them come to
// drift from the fourth.
const slotDeclarationPhrase = "declare `workshop_draft` under the phase's `slots`"

// The exact `slots` entry a phase writes is quoted ONCE, in the tools skill —
// the first phase of a build that touches the draft, so it is the first place
// the builder needs it — and the other three point at the plan tool's own
// guidance, which now carries the rule as the permission's planningNote
// (pkg/tools/workshopmcp/deploy/sidecartoolbox.yaml). Four copies of a JSON
// shape is four places to fix when it changes.
//
// The id-less form is the CORRECT one here, not a shortcut: `draft` is fixed
// by the toolbox, appears in no plan and in no argument, and an agent has no
// way to learn it. MissingSlotDeclarations refuses SILENCE about a declared
// type while permitting deferral, and planGateBindings then supplies the
// instance from the permission surface's own ConstantResourceID. A skill that
// taught `"id": "draft"` would be teaching a plan no builder can honestly
// write.
func TestToolsSkillQuotesTheSlotEntryShapeOnce(t *testing.T) {
	tools := collapseSpace(skillBody(t, "tools"))
	assert.Contains(t, tools, `{"type": "workshop_draft", "why":`,
		"the tools skill must show the entry itself, since it is the first draft-touching phase")
	assert.NotContains(t, tools, `"id": "draft"`,
		"the draft's name is not the agent's to write: it is supplied from the tool's own constant")

	for _, phase := range []string{"agent", "test", "handoff"} {
		t.Run(phase+": points at the plan tool's guidance instead of re-quoting the shape", func(t *testing.T) {
			body := collapseSpace(skillBody(t, phase))
			assert.NotContains(t, body, `{"type": "workshop_draft"`,
				"the shape is quoted once, in the tools skill")
			assert.Contains(t, body, "the plan tool's own guidance",
				"%s must send the builder to the guidance that carries the shape", phase)
		})
	}
}

// TestToolsSkillNamesTheArgumentGate pins the rule a live run showed was
// missing: the builder authored a tool with argument constraints and no
// allowedFields, so the platform's fail-closed argument gate refused every
// call the agent made — after the tool had been approved and installed.
// Nothing in the skill mentioned either field, so every tool it authored that
// took an argument would have failed the same way.
func TestToolsSkillNamesTheArgumentGate(t *testing.T) {
	body := skillBody(t, "tools")
	assert.Contains(t, body, "allowedFields",
		"the tools skill must name the field that lets a tool's arguments through")
	assert.Contains(t, body, "unconstrainedArgs",
		"and the explicit opt-out, since an empty allowedFields is not one")
	collapsed := strings.Join(strings.Fields(body), " ")
	assert.Contains(t, collapsed, "refuses every argument",
		"the body must say what a tool with neither actually does")
}

// TestAssessSkillAsksAboutPreferences pins the per-user-preferences interview
// question the assess phase gained: the people who use an agent may each set
// their own preferences for how it treats them, and the builder must offer
// that as a choice — especially for a team agent, where one person's settings
// should not silently become everyone's. Before this the builder never
// surfaced a whole class-authorable feature (userPreferences, landed
// 2026-09-16); a person had to know to ask for it by name.
func TestAssessSkillAsksAboutPreferences(t *testing.T) {
	body := collapseSpace(skillBody(t, "assess"))
	assert.Contains(t, body, "set their own preferences",
		"assess must offer per-user preferences as something to ask the person about")
}

// TestAgentSkillComposesUserPreferences pins the per-user-preferences
// composition item. It names the fields (userPreferences, and the self/class
// visibility axis) and the runtime tools (get_preferences to read, set_preference
// to save) so the builder composes the feature in one pass instead of
// discovering the shape through approval-gated apply-and-fail round trips — the
// exact cost the tools/agent/budget skills were already written to avoid. It
// also pins the two facts a builder most easily gets wrong: declaring the list
// is the whole opt-in (no separate capability), and the Slack settings pane is
// automatic (nothing extra to build).
func TestAgentSkillComposesUserPreferences(t *testing.T) {
	body := collapseSpace(skillBody(t, "agent"))
	for _, want := range []string{
		"userPreferences",
		"`visibility: self`",
		"`visibility: class`",
		"get_preferences",
		"set_preference",
		"no separate capability to turn on",
		"settings pane",
	} {
		assert.Contains(t, body, want, "agent must name %q when composing per-user preferences", want)
	}
}

// TestAgentSkillNamesSubagentModeCeiling pins the roster-delegation ceiling the
// agent skill gained: a hand-off's usage can be capped with subagentModes
// (single-turn vs an open back-and-forth), a recent authorable field the
// builder's roster guidance otherwise never named.
func TestAgentSkillNamesSubagentModeCeiling(t *testing.T) {
	body := collapseSpace(skillBody(t, "agent"))
	assert.Contains(t, body, "subagentModes",
		"agent must name the field that caps how a roster hand-off may be used")
}

// TestPermissionsSkillConfirmsPreferenceVisibility pins the one preference
// question that belongs in Permissions rather than Assess: whether a value one
// person sets may be read when the agent acts FOR someone else (visibility
// class) or stays private to each person (visibility self). It is a "who else
// might see what it does" decision, so the person confirms it here.
func TestPermissionsSkillConfirmsPreferenceVisibility(t *testing.T) {
	body := collapseSpace(skillBody(t, "permissions"))
	assert.Contains(t, body, "per-user preferences",
		"permissions must confirm the read-visibility of any per-user preferences the agent offers")
}

// TestToolsSkillNamesTheProviderCatalog pins the provider-catalog fix: a live
// builder invented sign-in ids ("github", "linear") because no skill named the
// closed set the platform ships, one approval-gated apply per guess until the
// toolguard breaker tripped (oap-desktop, 2026-09-13, TODO 590f06a29). The
// generic per-person OAuth sign-in is oauth-mcp, and the authoritative set is
// what workshop_inventory reports — named here so the builder reaches for a
// real id instead of one it made up.
func TestToolsSkillNamesTheProviderCatalog(t *testing.T) {
	body := collapseSpace(skillBody(t, "tools"))
	assert.Contains(t, body, "oauth-mcp",
		"tools must name the generic per-person OAuth sign-in, not leave the builder to invent one")
	assert.Contains(t, body, "workshop_inventory",
		"tools must send the builder to inventory for the sign-in flows this cluster ships")
}

// TestToolsSkillOnPassthroughTesting pins the correction to the "test each
// tool" step for per-person access: a userPassthrough tool cannot be
// test-called in the Tools phase because no account is linked in the workshop
// yet, so the builder must not treat that as a failure — it is exercised for
// real in the Test phase, once the person connects their own account. A live
// run silently skipped the test step here with no explanation (oap-desktop
// TODO 0e4a886a0).
func TestToolsSkillOnPassthroughTesting(t *testing.T) {
	body := collapseSpace(skillBody(t, "tools"))
	assert.Contains(t, body, "connects their own account",
		"tools must say why a per-person tool cannot be test-called here, not silently skip it")
}
