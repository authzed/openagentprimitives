// Package promptlib holds the prompt-building and JSON-response parsing
// helpers behind the toolspec llm.Provider. Backends differ on transport,
// credentials, and how they elicit JSON — some accept a response MIME type,
// others need instruct-the-model plus parse-text — but they share prompts and
// response schemas, so this package owns those. adapter.go turns any
// pkg/agent/llm provider into an llm.Provider using them.
package promptlib

import (
	"encoding/json"
	"fmt"
	"strings"

	toolspeccel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/llm"
)

// --- Phase 1: select toolkit ---

// BuildSelectPrompt returns (system, user) for the SelectToolkit call.
func BuildSelectPrompt(intent string, sums []llm.ToolkitSummary) (string, string) {
	var sb strings.Builder
	sb.WriteString("You are helping choose a CLI toolkit for a user's request.\n\n")
	sb.WriteString("Available toolkits:\n\n")
	for _, s := range sums {
		fmt.Fprintf(&sb, "# %s  (binary: %s)\n", s.Name, s.Binary)
		for _, sc := range s.Subcommands {
			tag := "read-only"
			if sc.Destructive {
				tag = "DESTRUCTIVE"
			}
			dest := ""
			if len(sc.NetworkDestinations) > 0 {
				dest = fmt.Sprintf("; network:[%s]", strings.Join(sc.NetworkDestinations, ", "))
			}
			writes := ""
			if len(sc.Writes) > 0 {
				writes = fmt.Sprintf("; writes:[%s]", strings.Join(sc.Writes, ", "))
			}
			fmt.Fprintf(&sb, "  %-20s %s%s%s\n", strings.Join(sc.Path, " "), tag, writes, dest)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(`Respond with JSON:
{
  "toolkitName": "<exact name from list, or empty string if none fit>",
  "reasoning":   "<one sentence explaining your pick>",
  "unmatched":   [{"request": "<fragment>", "reason": "<why no toolkit fits>"}]
}

Rules:
- Pick exactly one toolkit, or "" if none of them can represent this intent.
- "unmatched" is populated only when toolkitName is "".
- Never invent a toolkit name.
`)
	system := sb.String()
	user := fmt.Sprintf("User intent:\n\"\"\"\n%s\n\"\"\"\n", intent)
	return system, user
}

type selectJSON struct {
	ToolkitName string          `json:"toolkitName"`
	Reasoning   string          `json:"reasoning"`
	Unmatched   []llm.Unmatched `json:"unmatched"`
}

// ParseSelectResponse decodes a SelectToolkit JSON response.
func ParseSelectResponse(raw string) (*llm.SelectResponse, error) {
	cleaned := UnwrapSingleArray(StripFences(raw))
	var s selectJSON
	if err := json.Unmarshal([]byte(cleaned), &s); err != nil {
		return nil, fmt.Errorf("parse select response: %w (raw: %s)", err, raw)
	}
	return &llm.SelectResponse{
		ToolkitName: s.ToolkitName,
		Reasoning:   s.Reasoning,
		Unmatched:   s.Unmatched,
	}, nil
}

// --- Phase 2: generate spec ---

// BuildGeneratePrompt returns (system, user) for the GenerateSpec call.
func BuildGeneratePrompt(req llm.GenerateRequest) (string, string) {
	system := generateSystemPrompt()
	user := fmt.Sprintf(`User intent:
"""
%s
"""

Toolkit name: %s

Toolkit YAML:
"""
%s
"""
`, req.Intent, req.ToolkitName, req.ToolkitYAML)
	if req.PriorError != "" {
		user += fmt.Sprintf(`
Your previous attempt failed validation:
%s

Please fix the issue and emit a valid spec.
`, req.PriorError)
	}
	return system, user
}

type generateJSON struct {
	SpecYAML     string            `json:"specYAML"`
	Descriptions map[string]string `json:"descriptions"`
	Warnings     []string          `json:"warnings"`
	Excluded     []llm.Excluded    `json:"excluded"`
	Unmatched    []llm.Unmatched   `json:"unmatched"`
}

// ParseGenerateResponse decodes a GenerateSpec / RefineSpec JSON response.
func ParseGenerateResponse(raw string) (*llm.GenerateResponse, error) {
	cleaned := UnwrapSingleArray(StripFences(raw))
	var g generateJSON
	if err := json.Unmarshal([]byte(cleaned), &g); err != nil {
		return nil, fmt.Errorf("parse generate response: %w (raw: %s)", err, raw)
	}
	return &llm.GenerateResponse{
		SpecYAML:     g.SpecYAML,
		Descriptions: g.Descriptions,
		Warnings:     g.Warnings,
		Excluded:     g.Excluded,
		Unmatched:    g.Unmatched,
	}, nil
}

// --- Phase 3: test-case generation ---

// BuildTestGenPrompt returns (system, user) for the GenerateTestCases call.
func BuildTestGenPrompt(intent, toolkitName, toolkitYAML string) (string, string) {
	system := testGenSystemPrompt
	user := fmt.Sprintf(`User intent:
"""
%s
"""

Toolkit: %s

Toolkit YAML:
"""
%s
"""
`, intent, toolkitName, toolkitYAML)
	return system, user
}

type testGenJSON struct {
	TestCases []struct {
		Intent        string            `json:"intent"`
		Argv          []string          `json:"argv"`
		Env           map[string]string `json:"env"`
		Cwd           string            `json:"cwd"`
		BinaryVersion string            `json:"binaryVersion"`
		ExpectAllow   bool              `json:"expectAllow"`
	} `json:"testCases"`
}

// ParseTestGenResponse decodes a GenerateTestCases JSON response.
func ParseTestGenResponse(raw string) (*llm.TestResponse, error) {
	cleaned := UnwrapSingleArray(StripFences(raw))
	var t testGenJSON
	if err := json.Unmarshal([]byte(cleaned), &t); err != nil {
		return nil, fmt.Errorf("parse testgen response: %w (raw: %s)", err, raw)
	}
	out := make([]llm.TestCase, 0, len(t.TestCases))
	for _, c := range t.TestCases {
		out = append(out, llm.TestCase{
			Intent:        c.Intent,
			Argv:          c.Argv,
			Env:           c.Env,
			Cwd:           c.Cwd,
			BinaryVersion: c.BinaryVersion,
			ExpectAllow:   c.ExpectAllow,
		})
	}
	return &llm.TestResponse{TestCases: out}, nil
}

// --- Phase 4: refine spec ---

// BuildRefinePrompt returns (system, user) for the RefineSpec call.
func BuildRefinePrompt(req llm.RefineRequest) (string, string) {
	var mm strings.Builder
	for i, m := range req.Mismatches {
		fmt.Fprintf(&mm, "%d. Intent: %s\n   Argv: %v\n   Expected allow: %t\n   Actual allow:   %t\n   Reason: %s\n\n",
			i+1, m.TestCase.Intent, m.TestCase.Argv, m.TestCase.ExpectAllow, m.Actual, m.Reason)
	}
	var descBlock strings.Builder
	for k, v := range req.PriorDescriptions {
		fmt.Fprintf(&descBlock, "  %s: %s\n", k, v)
	}
	system := refineSystemPrompt
	user := fmt.Sprintf(`User intent:
"""
%s
"""

Toolkit: %s
Toolkit YAML:
"""
%s
"""

Prior spec:
"""
%s
"""

Prior descriptions:
"""
%s
"""

These test cases failed against the prior spec. Fix the spec (and adjust descriptions if the change warrants it) so that every case matches its expected allow value.

%s`, req.Intent, req.ToolkitName, req.ToolkitYAML, req.PriorSpecYAML, descBlock.String(), mm.String())
	if req.PriorError != "" {
		user += fmt.Sprintf(`
Your previous refinement failed validation:
%s

Please fix the issue and emit a valid spec.
`, req.PriorError)
	}
	return system, user
}

// --- helpers ---

// StripFences removes ```/```json code fences if present. A provider given a
// JSON response MIME type usually omits them; one steered only by an
// instruction emits them often, so every parser runs raw text through here.
func StripFences(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	lines := strings.SplitN(s, "\n", 2)
	if len(lines) < 2 {
		return s
	}
	s = lines[1]
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// UnwrapSingleArray turns "[ { ... } ]" into "{ ... }" so downstream
// parsers survive providers occasionally wrapping a single JSON object
// in a one-element array. Any other array shape is passed through
// unchanged (so the caller's parse error remains useful).
func UnwrapSingleArray(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
		return s
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(t), &arr); err != nil {
		return s
	}
	if len(arr) != 1 {
		return s
	}
	return string(arr[0])
}

// --- system prompts ---

// generateSystemPrompt returns the toolspec authoring system prompt
// with the CEL helper function list spliced in from
// pkg/tools/cel.HelperDocs(ScopeToolspec). Composing the prompt at call
// time (rather than baking it into a const) keeps the helper surface
// the LLM sees and the helper surface the runtime accepts in lock-step
// — registering a new helper in one place updates both.
func generateSystemPrompt() string {
	return generateSystemPromptHead +
		toolspeccel.HelperDocs(toolspeccel.ScopeToolspec) + "\n\n" +
		generateSystemPromptTail
}

const generateSystemPromptHead = `You are generating a ToolSpec that constrains how a CLI may be used.

The spec schema (YAML, with field types):
  name:            string (slug; required)
  version:         string (required; use "1")
  intent:          string (copy the user's intent verbatim)
  toolkit:
    name:          string (must match toolkit.name)
    revision:      string (must match toolkit.toolkitRevision)
  require:
    verifiedBinaryVersion: bool  (optional)
  allowSubcommands:  []string    (each is a space-joined subcommand path)
  deny:
    effects:
      destructive: bool          (true = block any subcommand marked destructive)
      reads:       []string      (values: "network" and/or "filesystem")
      writes:      []string      (values: "network" and/or "filesystem"; NOT a bool)
      creds:
        writes:    bool          (true = block any subcommand persisting creds)
  allow:
    network:
      destinations: []string     (hostname allowlist; e.g. ["api.github.com"])
    filesystem:
      pathsUnder:   []string     (path-prefix allowlist)
    creds:
      required:     []string     (env var names; must appear in toolkit.env.allowed)
  constraints:
    - cel:         string        (a CEL boolean expression over call.*)
      message:     string
  exceptions:
    - overrides:   []string      (rule paths)
      when:        string        (CEL boolean)
      message:     string

CEL surface (use these names EXACTLY — do not invent others like call.path or call.pos).

` + "`call`" + ` is a map variable with these field accesses (NOT functions):
  call.subcommand                     : string        (space-joined, e.g. "pr view")
  call.subcommandPath                 : list<string>  (e.g. ["pr", "view"])
  call.env[<var-name>]                : string
  call.positional[<name>]             : dyn           (BY NAME from toolkit.positional[].name,
                                                        NOT by numeric index — positional is a map)
  call.effects.destructive            : bool
  call.effects.writes                 : list<string>
  call.effects.network.destinations   : list<string>

Helper functions (registered in pkg/tools/cel; these are the ONLY function
names available — do not invent others like cron, regex.match, etc.):
`

const generateSystemPromptTail = `Examples of correct CEL for common patterns:
  # Constrain a flag to an allowlist (use hasFlag so it matches only when the flag is present):
  !call.hasFlag("repo") || call.flag("repo") in ["authzed/spicedb", "demo-org/demo-repo"]

  # Constrain a positional argument for a specific subcommand. If the toolkit declares
  # the positional as {name: repo, ...} for "repo view", use call.positional["repo"]:
  call.subcommand != "repo view" || call.positional["repo"] in ["authzed/spicedb", "demo-org/demo-repo"]

  # Combine a flag check AND a positional check across multiple subcommands:
  (!call.hasFlag("repo") || call.flag("repo") in ["a/b", "c/d"])
  && (call.subcommand != "repo view" || call.positional["repo"] in ["a/b", "c/d"])
  && (call.subcommand != "repo clone" || call.positional["repo"] in ["a/b", "c/d"])

Respond with JSON:
{
  "specYAML": "<full spec as YAML, JSON-string encoded>",
  "descriptions": {
    "allowSubcommands[0]": "explanation",
    "deny.effects.destructive": "explanation",
    "constraints[0]": "explanation"
  },
  "warnings":  ["approximation/caveat"],
  "excluded":  [{"name": "<subcommand>", "reason": "<why excluded>"}],
  "unmatched": [{"request": "<fragment>", "reason": "<why not representable>"}]
}

Rules:
- Use exact subcommand paths (space-joined) from the toolkit.
- Prefer structured deny.effects / allow.* over CEL whenever possible.
- Use CEL only for per-flag or per-env constraints that can't be expressed structurally.
- Every allowSubcommand, deny.effects.*, allow.*, constraint, and exception MUST have
  a matching entry in "descriptions". Use second-person voice ("you said…", "only this repo…").
- If the intent implies a capability no subcommand models, add a specific "unmatched" entry.
- Emit a complete YAML document inside specYAML. Do NOT wrap it in markdown fences.
- Respond with a SINGLE JSON object matching the schema. Do NOT wrap it in an array.
- "allowSubcommands" is a flat YAML sequence of strings. Each string is a space-joined
  subcommand path, e.g. "pr view". Do NOT emit it as a map or object.
- Per-subcommand descriptions go in the JSON "descriptions" field keyed by "allowSubcommands[N]",
  NEVER inline inside the allowSubcommands YAML.
- When the user names multiple values (e.g. "authzed/spicedb and demo-org/demo-repo"), express
  the bound as: call.flag("repo") in ["authzed/spicedb", "demo-org/demo-repo"]  — not a single-repo
  equality check.
- CEL constraints evaluate ONE invocation at a time. There is NO cross-call state — you
  cannot correlate flags or positionals across two different commands. A constraint like
  call.flag("repo") == call.flag("repo") is a tautology (always true) and useless.
  If the user says "stay on the same repo across commands", that is a CONSTRAINT TO PIN A
  SPECIFIC VALUE, not a cross-call equality check. Either pin the literal repo
  (call.flag("repo") == "owner/repo") or list the allowed values
  (call.flag("repo") in ["owner/a", "owner/b"]). If the user did not name a specific
  repo, add an "unmatched" entry explaining that pinning a single repo across
  invocations needs the user to name the repo.
- When in doubt, deny.`

const refineSystemPrompt = `You are fixing a ToolSpec that failed its test corpus.

You will be given:
  - the user's intent
  - the toolkit YAML
  - the prior generated spec YAML
  - the prior descriptions map
  - a list of failing test cases, each with Expected and Actual allow values and
    the validator's reason for the mismatch

For each mismatch, diagnose what in the prior spec caused it:
  - If the validator said "allowSubcommands" — the needed subcommand is missing from allowSubcommands.
  - If the validator said "deny.effects.*" — a structured deny is blocking a case the user wants allowed; narrow it.
  - If the validator said "allow.network.destinations" — a needed host is missing from the allow list.
  - If the validator said "constraints[N]" — a CEL predicate is too strict (common: single-repo check when the user named two repos; use "in [...]" or " || " instead of ==).
  - If a negative case (expectAllow=false) is being allowed — the spec is too permissive; tighten the rule.

Rewrite the spec YAML end-to-end (do not emit a patch). Keep descriptions aligned with the changes. Use the same JSON response shape as the initial generation (specYAML, descriptions, warnings, excluded, unmatched).

If a mismatch seems unsatisfiable given the intent, do NOT silently drop the test — add a "warnings" entry explaining why, and keep the test with its original expectAllow.

Respond with a SINGLE JSON object matching the schema below. Do NOT wrap it in an array.
Do NOT emit multiple alternatives. Do NOT wrap the JSON in markdown fences.`

const testGenSystemPrompt = `You are generating a test corpus that checks whether a ToolSpec faithfully represents a user's intent.

Each test case is a proposed CLI invocation with an expected allow/deny outcome.

Respond with JSON:
{
  "testCases": [
    {
      "intent": "<one-line description of this case>",
      "argv":   ["<token>", "..."],
      "env":    {"<VAR>": "<value>"},
      "cwd":    "<working dir, optional>",
      "binaryVersion": "<semver, optional>",
      "expectAllow": true
    }
  ]
}

Guidelines:
- Produce 5 to 12 cases. Include positive cases (should allow), negative cases (should deny),
  and boundary cases (wrong flag, destructive variant, missing version).
- Each case must use real subcommand paths / flags / env var names from the toolkit.
- Every case must have an explicit expectAllow boolean.
- Be specific about why each case is included; put that reason in "intent" (second-person voice).
- "argv" MUST start with the first subcommand token — NOT the binary name.
  For the command "gh pr view 123 -R org/repo", argv is ["pr", "view", "123", "-R", "org/repo"]
  (NOT ["gh", "pr", "view", "123", "-R", "org/repo"]).
  For the command "kubectl get pods -n tenant-foo", argv is ["get", "pods", "-n", "tenant-foo"].
- "binaryVersion" should be a real semver that falls inside the toolkit's target.versionRange
  when provided. If omitted, verifiedBinaryVersion checks may surface a warning.`
