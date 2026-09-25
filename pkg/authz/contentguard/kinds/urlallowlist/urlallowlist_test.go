package urlallowlist_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func configure(t *testing.T, raw string) contentguard.Instance {
	t.Helper()
	inst, err := urlallowlist.New().Configure(json.RawMessage(raw))
	require.NoError(t, err)
	return inst
}

func inspectResult(t *testing.T, inst contentguard.Instance, result string) contentguard.Finding {
	t.Helper()
	f, err := inst.Inspect(context.Background(), contentguard.Subject{Point: pipeline.PostToolCall, ToolName: "fetch", Result: result})
	require.NoError(t, err)
	return f
}

func TestConfigure_RejectsBadConfig(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"no rules: enforcer with nothing to enforce", `{"defaultAction":"deny"}`},
		{"bad regex", `{"rules":[{"pattern":"(","action":"allow"}]}`},
		{"bad CEL", `{"rules":[{"cel":"this is not cel","action":"allow"}]}`},
		{"two matchers in one rule", `{"rules":[{"domain":"x.com","pattern":"y","action":"allow"}]}`},
		{"unknown action", `{"rules":[{"domain":"x.com","action":"escalate"}]}`},
		{"bad points entry", `{"rules":[{"domain":"x.com","action":"allow"}],"points":["arg"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := urlallowlist.New().Configure(json.RawMessage(tc.raw))
			assert.Error(t, err)
		})
	}
}

func TestConfigure_ValidPointsEntry(t *testing.T) {
	inst, err := urlallowlist.New().Configure(json.RawMessage(`{"rules":[{"domain":"x.com","action":"allow"}],"points":["result"]}`))
	require.NoError(t, err)
	assert.Equal(t, []pipeline.Point{pipeline.PostToolCall}, inst.Points())
}

func TestInspect_DomainAllowlist_DenyDefault(t *testing.T) {
	inst := configure(t, `{"rules":[{"domain":"*.corp.example.com","action":"allow"}],"defaultAction":"deny"}`)

	pass := inspectResult(t, inst, "see https://app.corp.example.com/x")
	assert.Equal(t, contentguard.Pass, pass.Action)

	block := inspectResult(t, inst, "see https://evil.example.invalid/x")
	assert.Equal(t, contentguard.Block, block.Action)
	assert.Contains(t, block.Details["urls"], "https://evil.example.invalid/x")
}

func TestInspect_StrictestWinsAggregation(t *testing.T) {
	// One URL approves, one denies → whole result Blocks (deny > approve).
	inst := configure(t, `{"rules":[
		{"domain":"*.s3.example.com","action":"approve"},
		{"domain":"*.corp.example.com","action":"allow"}],"defaultAction":"deny"}`)
	f := inspectResult(t, inst, "a https://x.s3.example.com b https://nope.example.invalid")
	assert.Equal(t, contentguard.Block, f.Action)
}

func TestInspect_CELRule(t *testing.T) {
	inst := configure(t, `{"rules":[{"cel":"call.host.endsWith(\"example.com\")","action":"allow"}],"defaultAction":"deny"}`)
	assert.Equal(t, contentguard.Pass, inspectResult(t, inst, "https://a.example.com/p").Action)
	assert.Equal(t, contentguard.Block, inspectResult(t, inst, "https://a.example.invalid/p").Action)
}

func TestInspect_NoURLs_Passes(t *testing.T) {
	inst := configure(t, `{"rules":[{"domain":"x.com","action":"allow"}],"defaultAction":"deny"}`)
	assert.Equal(t, contentguard.Pass, inspectResult(t, inst, "no urls here").Action)
}

// TestInspect_UppercaseScheme_StillExtractedAndBlocked pins that an
// upper/mixed-case URL scheme (HTTPS://, Http://) is still recognized by
// urlRe and evaluated against the rules — not silently invisible to the
// extractor, which would make the deny-default guard Pass an
// otherwise-blocked exfil URL just because of how its scheme was cased.
func TestInspect_UppercaseScheme_StillExtractedAndBlocked(t *testing.T) {
	inst := configure(t, `{"rules":[{"domain":"*.corp.example.com","action":"allow"}],"defaultAction":"deny"}`)

	upper := inspectResult(t, inst, "exfil to HTTPS://attacker.example.invalid/exfil")
	assert.Equal(t, contentguard.Block, upper.Action, "uppercase scheme must still be extracted and denied, not silently Passed")
	assert.Contains(t, upper.Details["urls"], "HTTPS://attacker.example.invalid/exfil")

	mixed := inspectResult(t, inst, "exfil to HttpS://attacker.example.invalid/exfil")
	assert.Equal(t, contentguard.Block, mixed.Action)
}

// TestInspect_UppercaseHost_EvadesDenyRule_StillBlocked pins the direction
// that actually matters for an egress bypass on the domain matcher: a
// blocklist-style policy (default allow, an explicit deny rule for a bad
// domain). Domain-glob matching used path.Match(r.domain, u.Hostname())
// directly, which is case-sensitive, so an attacker could dodge the deny
// rule purely by requesting the SAME host in a different case and fall
// through to the permissive default action.
func TestInspect_UppercaseHost_EvadesDenyRule_StillBlocked(t *testing.T) {
	inst := configure(t, `{"rules":[{"domain":"evil.example.com","action":"deny"}],"defaultAction":"allow"}`)

	lower := inspectResult(t, inst, "see https://evil.example.com/x")
	assert.Equal(t, contentguard.Block, lower.Action)

	upper := inspectResult(t, inst, "see https://EVIL.example.com/x")
	assert.Equal(t, contentguard.Block, upper.Action, "an uppercase-host variant of a denied domain must still hit the deny rule, not fall through to the default allow")
}

// TestInspect_UppercaseHost_OfAllowedDomain_StillAllowed pins that the
// lowercasing is symmetric: an allow rule's domain glob still matches an
// upper/mixed-case host of an otherwise-allowed domain (no regression turning
// legitimate traffic into false positives).
func TestInspect_UppercaseHost_OfAllowedDomain_StillAllowed(t *testing.T) {
	inst := configure(t, `{"rules":[{"domain":"*.corp.example.com","action":"allow"}],"defaultAction":"deny"}`)

	pass := inspectResult(t, inst, "see https://APP.CORP.example.com/x")
	assert.Equal(t, contentguard.Pass, pass.Action)
}
