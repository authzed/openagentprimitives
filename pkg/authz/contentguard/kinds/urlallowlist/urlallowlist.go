// Package urlallowlist is a content-guard inspector enforcing a URL policy on
// tool I/O: an ordered, first-match-wins rule list (like ToolGuardPolicy), each
// rule matching a URL by domain glob / regex / CEL and carrying its own
// allow|deny|approve action. Per-URL actions are folded to one wholesale result
// action by strictest-wins (deny > approve > allow).
package urlallowlist

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/google/cel-go/cel"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	celpkg "github.com/authzed/openagentprimitives/pkg/tools/cel"
)

const id = "url-allowlist"

type Config struct {
	Rules         []URLRule `json:"rules"`
	DefaultAction string    `json:"defaultAction,omitempty"` // allow|deny|approve (default deny)
	Points        []string  `json:"points,omitempty"`        // args|result (default both)
}

type URLRule struct {
	// Domain matches the URL's host exactly; empty means this rule ignores host.
	Domain string `json:"domain,omitempty"`
	// Pattern is a glob over the whole URL; empty means no pattern test.
	Pattern string `json:"pattern,omitempty"`
	// CEL is an expression over the URL; empty means no expression test.
	CEL string `json:"cel,omitempty"`
	// Action taken on a match: allow|deny|approve. Required.
	Action string `json:"action"`
}

// Inspector is the registered factory.
type Inspector struct{}

func New() *Inspector { return &Inspector{} }

func (Inspector) ID() string { return id }

func init() { registry.Register(New()) }

// compiledRule pairs a precompiled matcher with its action.
type compiledRule struct {
	action  contentguard.Action
	domain  string // host glob (path.Match)
	pattern *regexp.Regexp
	prog    cel.Program
}

type instance struct {
	rules  []compiledRule
	def    contentguard.Action
	points []pipeline.Point
}

// (?i) makes the scheme match case-insensitively (HTTPS://, Http://, …) — a
// tool result or arg can embed a URL in any case, and without this an
// upper/mixed-case scheme is invisible to urlRe.FindAllString, so no rule
// (allow or deny) ever matches it and the deny-default guard silently Passes
// what should be blocked. See compiledRule.matches for the matching host
// lowercasing that closes the same gap for domain rules.
var urlRe = regexp.MustCompile(`(?i)https?://[^\s"'<>)\]]+`)

func parseAction(s string) (contentguard.Action, error) {
	switch s {
	case "allow":
		return contentguard.Pass, nil
	case "deny":
		return contentguard.Block, nil
	case "approve":
		return contentguard.Approve, nil
	default:
		return 0, fmt.Errorf("action %q is not allow|deny|approve", s)
	}
}

func (Inspector) Configure(raw json.RawMessage) (contentguard.Instance, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("url-allowlist: parse config: %w", err)
	}
	if len(cfg.Rules) == 0 {
		return nil, fmt.Errorf("url-allowlist: at least one rule is required (an enabled guard with no rules is a misconfig)")
	}
	def := contentguard.Block // default action "deny"
	if cfg.DefaultAction != "" {
		a, err := parseAction(cfg.DefaultAction)
		if err != nil {
			return nil, fmt.Errorf("url-allowlist: defaultAction: %w", err)
		}
		def = a
	}
	inst := &instance{def: def}
	for i, r := range cfg.Rules {
		n := 0
		if r.Domain != "" {
			n++
		}
		if r.Pattern != "" {
			n++
		}
		if r.CEL != "" {
			n++
		}
		if n != 1 {
			return nil, fmt.Errorf("url-allowlist: rule[%d] must set exactly one of domain|pattern|cel", i)
		}
		act, err := parseAction(r.Action)
		if err != nil {
			return nil, fmt.Errorf("url-allowlist: rule[%d].action: %w", i, err)
		}
		// Lowercase the domain glob at compile time so it compares equal to the
		// lowercased Hostname() in compiledRule.matches regardless of the case
		// the rule author wrote it in (host names are case-insensitive per RFC
		// 4343, and path.Match is not).
		cr := compiledRule{action: act, domain: strings.ToLower(r.Domain)}
		if r.Pattern != "" {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				return nil, fmt.Errorf("url-allowlist: rule[%d].pattern: %w", i, err)
			}
			cr.pattern = re
		}
		if r.CEL != "" {
			prog, err := celpkg.Compile(r.CEL)
			if err != nil {
				return nil, fmt.Errorf("url-allowlist: rule[%d].cel: %w", i, err)
			}
			cr.prog = prog
		}
		inst.rules = append(inst.rules, cr)
	}
	pts, err := parsePoints(cfg.Points)
	if err != nil {
		return nil, err
	}
	inst.points = pts
	return inst, nil
}

func parsePoints(ps []string) ([]pipeline.Point, error) {
	if len(ps) == 0 {
		return []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}, nil
	}
	var out []pipeline.Point
	for _, p := range ps {
		switch p {
		case "args":
			out = append(out, pipeline.PreToolCall)
		case "result":
			out = append(out, pipeline.PostToolCall)
		default:
			return nil, fmt.Errorf("url-allowlist: points entry %q is not args|result", p)
		}
	}
	return out, nil
}

func (i *instance) Points() []pipeline.Point { return i.points }

// InspectsWholeContent declares this inspector exempt from
// contentguard.MaxInspectBytes (contentguard.WholeContentInspector).
//
// The cap exists so a padded payload cannot stall the prompt-injection
// detector's HTTP round trip past its timeout and collect that inspector's
// warn-mode Pass. Nothing here can be stalled: Inspect is an in-process RE2
// scan — no network hop, no timeout, no context deadline, no error path at all,
// and RE2 is linear in the input. So a cap could not protect this inspector
// from anything; it could only hide URLs from it.
//
// And hiding a URL here is an egress. The shipped default is points
// [args, result] with defaultAction deny, and the meta tools that reach the
// outside world — respond_to_user, artifact_prepare — are inspected on their
// ARGS, which the (injectable) model authors and can pad at will. Capped, this
// guard's deny-by-default would silently become a Pass for any call larger than
// the cap: the biggest payloads, least scanned, which is the inverse of the
// intent.
func (*instance) InspectsWholeContent() bool { return true }

func (i *instance) Inspect(_ context.Context, s contentguard.Subject) (contentguard.Finding, error) {
	// Subject.Text is the point's content (result or serialized args). This
	// inspector declares InspectsWholeContent, so it is the whole of it.
	urls := urlRe.FindAllString(s.Text(), -1)

	// Fold per-URL actions to one result action by strictest-wins:
	// deny > approve > allow. NOTE: the Action iota is Pass<Block<Approve, which
	// is NOT the severity order — compute it explicitly, never by comparing ints.
	worst := contentguard.Pass
	sawApprove := false
	var offending []string
	for _, u := range urls {
		switch i.actionFor(u) {
		case contentguard.Block:
			worst = contentguard.Block
			offending = append(offending, u)
		case contentguard.Approve:
			sawApprove = true
			offending = append(offending, u)
		}
	}
	if worst != contentguard.Block && sawApprove {
		worst = contentguard.Approve
	}
	if worst == contentguard.Pass {
		return contentguard.Finding{Action: contentguard.Pass}, nil
	}
	reason := fmt.Sprintf("url-allowlist: %d non-allowlisted URL(s)", len(offending))
	return contentguard.Finding{Action: worst, Reason: reason, Details: map[string]any{"urls": offending}}, nil
}

func (i *instance) actionFor(rawURL string) contentguard.Action {
	u, err := url.Parse(rawURL)
	if err != nil {
		return i.def // unparseable → default (deny by default)
	}
	for _, r := range i.rules {
		if r.matches(rawURL, u) {
			return r.action
		}
	}
	return i.def
}

func (r compiledRule) matches(rawURL string, u *url.URL) bool {
	switch {
	case r.domain != "":
		// u.Hostname() preserves the case from the raw URL (e.g.
		// HTTPS://ATTACKER/exfil); lowercase it to match the lowercased r.domain
		// so a mixed/upper-case host can't dodge an allow OR a deny rule.
		ok, _ := path.Match(r.domain, strings.ToLower(u.Hostname()))
		return ok
	case r.pattern != nil:
		return r.pattern.MatchString(rawURL)
	case r.prog != nil:
		call := map[string]any{
			"url": rawURL, "scheme": u.Scheme, "host": u.Hostname(),
			"path": u.Path, "query": u.RawQuery,
		}
		ok, err := celpkg.EvalBool(r.prog, call, nil)
		return err == nil && ok
	}
	return false
}
