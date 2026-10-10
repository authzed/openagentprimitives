// Package haiku is the Haiku-backed ConsequenceJudge: the PRIMARY classifier
// for Phase E's browser acting gate (per controller ruling E-R3 — the model
// is the real classifier; E6's deterministic judge is only a fast-path for
// the obvious cases it positively recognizes). It sends one action's DOM
// facts to a small, cheap model and asks it to classify the action into the
// same four-tier taxonomy judge.go defines, returning a full probability
// distribution (not just a label) so a caller can threshold
// `ask if P(mutate) >= T` instead of trusting a bare argmax.
//
// The model NEVER authorizes anything here — Classify returns a
// classification, and the caller (E8) is the one that turns a tier/threshold
// into an allow/gate decision. This package only translates one Action into a
// prompt and one model response back into a Verdict.
package haiku

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/providers"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"

	// Blank-imported so providers.New("anthropic", apiKey) always resolves
	// regardless of what else a binary happens to import first. Unlike a
	// channel transport or a toolspec kind — where an operator genuinely
	// chooses which backends to install — this backend's whole identity is
	// "anthropic-haiku": there is no variant of this judge that is NOT backed
	// by the anthropic provider, so depending on some other file in the binary
	// to have already imported it would be fragile. Mirrors the credkind/imports
	// exception documented in AGENTS.md (pkg/platform/identity/broker/inproc).
	_ "github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
)

// Name is this backend's registration name (judge.Register / judge.Get).
const Name = "anthropic-haiku"

// DefaultModelID is the Haiku model id used when a caller does not override
// it. Chosen for cost/latency: this backend runs on every acting call the
// deterministic fast-path does not positively resolve, so it needs to be
// cheap. See pkg/agent/llm/models.Anthropic for pricing.
const DefaultModelID = "claude-haiku-4-5"

// DefaultMutateThreshold is the default policy threshold for "ask the user if
// P(mutate) >= T" that E8 applies to this judge's Verdict.Probs.
//
// 0.15, chosen against REAL Haiku numbers (not a guess). The task-52 fix first
// raised this from 0.15 to 0.30 on the theory that the new deterministic clears
// (any `type`, a radio/checkbox/option toggle) remove the benign field-edits
// before the model is consulted, so a higher cut would shed over-asks. The
// env-gated live eval (haiku_corpus_eval_test.go, run against claude-haiku-4-5
// over the 54-case corpus) DISPROVED that: the over-ask count is already 0 at
// 0.15 (the deterministic clears + the improved prompt push real form-filling
// to ~0.05 on mutate), so raising the cut buys no over-ask reduction — it only
// ADDS escapes. The measured sweep:
//
//     T=0.10   auto-allowed 33/54   escapes 1   over-asks 5
//     T=0.15   auto-allowed 38/54   escapes 1   over-asks 0   <- optimal
//     T=0.20   auto-allowed 38/54   escapes 2   over-asks 0
//     T=0.30   auto-allowed 38/54   escapes 2   over-asks 0
//
// 0.15 is the strict optimum: 0 over-asks AND the fewest escapes. The single
// escape at 0.15 is "grant OAuth app permissions" (Haiku rates it session,
// P=0.05) — a classifier miss no threshold catches. Raising to 0.20+ adds a
// SECOND escape, the adversarial "'Read more' link that hits a delete endpoint"
// (P=0.15) — a genuine deceptive-UI commit that 0.15 catches and 0.30 lets
// through. So the over-gating fix's VALUE is its deterministic/prompt changes,
// not a threshold raise; the cut stays at the original 0.15.
//
// FAIL-CLOSED: lower is safer here (more gold-mutate caught). 0.10 was rejected
// only because it reintroduces over-asks (5) with no escape benefit. Re-run the
// live eval if the corpus or the prompt changes; threshold_test.go pins the
// browser tool's copy equal to this constant.
const DefaultMutateThreshold = 0.15

// maxResponseTokens bounds the classification response. The JSON payload is
// small (a tier string, four floats, a short rationale sentence); this is
// generous headroom, not a tuned budget.
const maxResponseTokens = 512

// Judge is the Haiku-backed ConsequenceJudge. Construct directly via New to
// inject a Provider (tests use a fake — see haiku_test.go); production
// resolves the anthropic provider via NewFromAPIKey, or indirectly through
// this package's init() registration (register.go), which resolves
// ANTHROPIC_API_KEY lazily on first Classify call.
type Judge struct {
	prov  llm.Provider
	model string
}

// New constructs a Judge over the given llm.Provider. An empty model falls
// back to DefaultModelID.
func New(prov llm.Provider, model string) *Judge {
	if model == "" {
		model = DefaultModelID
	}
	return &Judge{prov: prov, model: model}
}

// NewFromAPIKey constructs the production Judge: resolves the "anthropic"
// llm.Provider via the providers registry (so this backend is wired through
// the same seam every other provider consumer uses — see AGENTS.md's
// Pluggability table) and pins DefaultModelID.
func NewFromAPIKey(apiKey string) (*Judge, error) {
	prov, err := providers.New("anthropic", apiKey)
	if err != nil {
		return nil, fmt.Errorf("haiku judge: resolve anthropic provider: %w", err)
	}
	return New(prov, DefaultModelID), nil
}

// Name implements judge.ConsequenceJudge.
func (j *Judge) Name() string { return Name }

// Classify implements judge.ConsequenceJudge. FAIL-CLOSED CONTRACT: any
// failure here — a Send error, a response with no usable text, an
// unparseable or incomplete JSON payload, a missing/unrecognized tier — is
// returned as an error, never silently defaulted to a Verdict. Per judge.go's
// documented contract, an error means THIS JUDGE failed; the caller (E8) is
// what maps a judge error to the fail-closed TierMutate outcome. This
// function does not perform that mapping itself, so a bug here cannot
// accidentally wave an action through.
func (j *Judge) Classify(ctx context.Context, a judge.Action, page judge.PageContext) (judge.Verdict, error) {
	if j == nil || j.prov == nil {
		return judge.Verdict{}, fmt.Errorf("haiku judge: no llm.Provider configured")
	}

	user, err := buildUserPrompt(a, page)
	if err != nil {
		return judge.Verdict{}, fmt.Errorf("haiku judge: %w", err)
	}

	resp, err := j.prov.Send(ctx, llm.Request{
		Model: j.model,
		System: []llm.SystemBlock{{
			Text:      systemPrompt,
			Cacheable: true,
		}},
		Messages: []llm.Message{{
			Role: "user",
			Content: []llm.ContentBlock{{
				Type: "text",
				Text: user,
			}},
		}},
		MaxTokens: maxResponseTokens,
	})
	if err != nil {
		return judge.Verdict{}, fmt.Errorf("haiku judge: %s send: %w", j.prov.Name(), err)
	}

	raw, err := firstText(resp)
	if err != nil {
		return judge.Verdict{}, fmt.Errorf("haiku judge: %w", err)
	}

	v, err := parseVerdict(raw)
	if err != nil {
		return judge.Verdict{}, fmt.Errorf("haiku judge: %w", err)
	}
	return v, nil
}

// --- prompt construction ---

// classificationCriteria is the 4-tier taxonomy, VERBATIM from
// ~/Projects/laya/spike_gate2.py's CHOICE_Q["tier"]["criteria"] (the
// "choice" primitive that spike found outperformed a rounded ordinal score —
// see spike_gate2.py's comparison A-E). Order matters: it is rendered into
// the prompt in this order, and allTiers (below) mirrors it so Verdict.Probs
// always carries all four keys.
var classificationCriteria = []struct {
	tier judge.Tier
	text string
}{
	{judge.TierObserve, "reading, scrolling, hovering, expanding or copying visible content; nothing changes"},
	{judge.TierNavigate, "following links, searching, filtering, paginating, typing queries; ephemeral, nothing persists"},
	{judge.TierSession, "logging in or out, consent banners, verification codes; stateful but nothing the user owns changes"},
	{judge.TierMutate, "changes, creates, deletes, sends, posts, books, or purchases something belonging to the user"},
}

// allTiers is the canonical tier order, derived from classificationCriteria
// so there is exactly one place that enumerates "the four tiers."
var allTiers = func() []judge.Tier {
	out := make([]judge.Tier, len(classificationCriteria))
	for i, c := range classificationCriteria {
		out[i] = c.tier
	}
	return out
}()

// systemPrompt is built once at package init from classificationCriteria plus
// the output-format contract. "An automated agent is about to perform this
// browser action on the user's behalf. Classify the action." is also
// verbatim from spike_gate2.py's CHOICE_Q instructions.
var systemPrompt = buildSystemPrompt()

func buildSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("An automated agent is about to perform this browser action on the user's behalf. Classify the action.\n\n")
	sb.WriteString("Classify the action into exactly one of these four tiers:\n\n")
	for _, c := range classificationCriteria {
		fmt.Fprintf(&sb, "- %s: %s\n", c.tier, c.text)
	}
	sb.WriteString("\nDistinguish FILLING a form from COMMITTING it. Typing into a field, " +
		"selecting a radio button or checkbox, picking a dropdown option, and focusing a " +
		"field change only LOCAL page state -- nothing is sent to the server -- so they are " +
		"navigate (or observe), NOT mutate, even when the surrounding form posts to a " +
		"mutating-looking path like /post, /order, or /checkout. The MUTATE is the separate, " +
		"later action that COMMITS the form: submitting it, placing or confirming an order, " +
		"paying, deleting, sending, or posting. The action facts carry " +
		"\"is_submit_control\": true when the target is a form's submit control (a submit or " +
		"image button, or a bare <button> inside a form); a click or Enter on such a control " +
		"COMMITS the form and is mutate -- UNLESS the form only searches, filters, or " +
		"paginates, which stays navigate.\n\n")
	sb.WriteString("You are a CLASSIFIER ONLY. You do not grant, deny, or approve anything -- " +
		"a separate authorization layer makes that decision from your output. Report your " +
		"honest belief, including genuine uncertainty: when an action could plausibly fall " +
		"into more than one tier, spread your \"probs\" distribution to reflect that instead " +
		"of collapsing to one tier with artificial confidence. An action that is ambiguous or " +
		"whose consequence you are not sure of should carry real probability mass on \"mutate\" " +
		"rather than being rounded away.\n\n")
	sb.WriteString("Respond with a JSON object ONLY -- no prose, no explanations, no markdown code " +
		"fences. Begin your response with { and end it with }. The JSON MUST have exactly this " +
		"shape:\n\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"tier\": \"<one of: observe, navigate, session, mutate -- your single best classification>\",\n")
	sb.WriteString("  \"probs\": {\n")
	sb.WriteString("    \"observe\": <float 0..1>,\n")
	sb.WriteString("    \"navigate\": <float 0..1>,\n")
	sb.WriteString("    \"session\": <float 0..1>,\n")
	sb.WriteString("    \"mutate\": <float 0..1>\n")
	sb.WriteString("  },\n")
	sb.WriteString("  \"rationale\": \"<one short sentence explaining the classification>\"\n")
	sb.WriteString("}\n\n")
	sb.WriteString("The four \"probs\" values must all be present and should sum to approximately 1.0.")
	return sb.String()
}

// actionFactsForPrompt is the JSON shape sent to the model: the Action's DOM
// facts plus the supporting PageContext. Field names are snake_case to match
// this backend's own prompt convention (not Action's Go field names), kept
// stable since a prompt change is a behavior change worth noticing in review.
type actionFactsForPrompt struct {
	Op                string   `json:"op"`
	Role              string   `json:"role,omitempty"`
	Text              string   `json:"text,omitempty"`
	FormAction        string   `json:"form_action,omitempty"`
	IsSubmitControl   bool     `json:"is_submit_control,omitempty"`
	Container         string   `json:"container,omitempty"`
	Host              string   `json:"host,omitempty"`
	PageURL           string   `json:"page_url,omitempty"`
	URLPath           string   `json:"url_path,omitempty"`
	VisibleTextSample []string `json:"visible_text_sample,omitempty"`
}

func buildUserPrompt(a judge.Action, page judge.PageContext) (string, error) {
	facts := actionFactsForPrompt{
		Op:                a.Op,
		Role:              a.Role,
		Text:              a.Text,
		FormAction:        a.FormAction,
		IsSubmitControl:   a.Submits,
		Container:         a.Container,
		Host:              a.Host,
		PageURL:           a.PageURL,
		URLPath:           page.URLPath,
		VisibleTextSample: page.VisibleTextSample,
	}
	raw, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal action facts: %w", err)
	}
	return "Classify this browser action:\n\n" + string(raw), nil
}

// --- response parsing ---

func firstText(resp llm.Response) (string, error) {
	for _, b := range resp.Content {
		if b.Type == "text" && b.Text != "" {
			return b.Text, nil
		}
	}
	return "", fmt.Errorf("provider returned no text content (stop_reason=%q)", resp.StopReason)
}

// stripFences removes ```/```json code fences if present. Mirrors
// pkg/tools/toolspec/llm/promptlib.StripFences's behavior, kept as a small
// local copy rather than an import: that package belongs to the toolspec
// authoring pipeline (a different pkg/tools concern), and judge.go
// deliberately keeps this package's dependency surface to pkg/agent/llm +
// its own types (see judge.go's package doc) rather than reaching into
// pkg/tools for a five-line string helper.
func stripFences(raw string) string {
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

// verdictJSON is the wire shape parseVerdict unmarshals the model's response
// into, before validating it into a judge.Verdict.
type verdictJSON struct {
	Tier      string             `json:"tier"`
	Probs     map[string]float64 `json:"probs"`
	Rationale string             `json:"rationale"`
}

// parseTier maps a (trimmed, lowercased) tier string onto a judge.Tier, ok=
// false for anything that is not one of the four known tiers -- including an
// empty string, which is how a model response missing the "tier" field
// unmarshals.
func parseTier(s string) (judge.Tier, bool) {
	t := judge.Tier(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range allTiers {
		if t == known {
			return t, true
		}
	}
	return "", false
}

// parseVerdict parses and validates the model's raw text response into a
// Verdict. Every failure mode here is deliberately an error, not a default:
// invalid JSON, a missing/unrecognized tier, an absent probs object, or a
// probs object missing any of the four tier keys. This is what makes
// Classify's fail-closed contract hold even when the model itself
// misbehaves (hallucinated field names, a fifth tier, a truncated response)
// rather than only when the network call itself errors.
func parseVerdict(raw string) (judge.Verdict, error) {
	clean := stripFences(raw)

	var vj verdictJSON
	if err := json.Unmarshal([]byte(clean), &vj); err != nil {
		return judge.Verdict{}, fmt.Errorf("invalid JSON response (%w): %s", err, truncate(clean, 200))
	}

	tier, ok := parseTier(vj.Tier)
	if !ok {
		return judge.Verdict{}, fmt.Errorf("missing or unrecognized tier %q", vj.Tier)
	}

	if len(vj.Probs) == 0 {
		return judge.Verdict{}, fmt.Errorf("response has no probs distribution")
	}
	probs := make(map[judge.Tier]float64, len(allTiers))
	for _, t := range allTiers {
		p, ok := vj.Probs[string(t)]
		if !ok {
			return judge.Verdict{}, fmt.Errorf("probs missing tier %q", t)
		}
		// A probability must be a real value in [0,1]. A model that reports a
		// negative, >1, or NaN/Inf "probability" has misbehaved exactly like one
		// reporting a fifth tier or a missing key — so it is an error, not a value
		// to clamp-and-trust. Rejecting it keeps Classify's fail-closed contract:
		// the caller maps this error to the gated TierMutate outcome rather than
		// thresholding a garbage number (e.g. a NaN, for which every comparison is
		// false, would slip a mutate past a `>= threshold` gate).
		if p < 0 || p > 1 || math.IsNaN(p) || math.IsInf(p, 0) {
			return judge.Verdict{}, fmt.Errorf("probs[%q] out of range: %v (must be a real value in [0,1])", t, p)
		}
		probs[t] = p
	}

	return judge.Verdict{
		Tier:      tier,
		Probs:     probs,
		Rationale: vj.Rationale,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
