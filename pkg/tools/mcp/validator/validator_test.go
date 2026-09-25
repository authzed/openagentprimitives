package validator_test

import (
	"encoding/json"
	"testing"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func minimalSpec() *mcpspec.Spec {
	return &mcpspec.Spec{
		Name:    "test",
		Version: "1",
		Server: mcpspec.Server{
			URL:       "https://example.com/mcp",
			Transport: mcpspec.TransportStreamableHTTP,
		},
		Tools: []mcpspec.Tool{
			{Name: "alpha"},
			{Name: "beta"},
		},
	}
}

func specWithAllowedFields(t *testing.T) *mcpspec.Spec {
	t.Helper()
	sp := minimalSpec()
	sp.Tools[0].Args = mcpspec.Args{
		AllowedFields: []string{"foo", "bar"},
	}
	return sp
}

func specWithConstraint(t *testing.T, expr, msg string) *mcpspec.Spec {
	t.Helper()
	sp := minimalSpec()
	// UnconstrainedArgs isolates the constraints phase: these cases pass a
	// free-form `repo` arg, which the fail-closed allowedFields phase would
	// otherwise deny before constraints run.
	sp.Tools[0].Args.UnconstrainedArgs = true
	sp.Tools[0].Args.Constraints = []toolspec.Constraint{{CEL: expr, Message: msg}}
	return sp
}

// TestCheck exercises Check across the per-phase deny/allow paths that the
// validator wires today: tool allowlist, allowedFields, CEL constraints, and
// deny.effects. Each row builds a spec, fires Check with an invocation, and
// runs row-specific assertions on the resulting Decision/error.
func TestCheck(t *testing.T) {
	cases := []struct {
		name       string
		spec       func(t *testing.T) *mcpspec.Spec
		invocation validator.Invocation
		check      func(t *testing.T, d *validator.Decision, err error)
	}{
		{
			name: "tool phase: name not in allowlist denies with path=tool",
			spec: func(*testing.T) *mcpspec.Spec { return minimalSpec() },
			invocation: validator.Invocation{
				ToolName: "gamma",
				Args:     map[string]any{},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.False(t, d.Allow, "expected Allow=false for unknown tool")
				require.NotNil(t, d.FailedOn, "FailedOn should be set on deny")
				assert.Equal(t, "tool", d.FailedOn.Path)
				assert.Contains(t, d.Reason, "gamma", "Reason should mention the rejected tool name")
				require.NotNil(t, d.Parsed, "ParsedArgs should be populated even on deny")
				assert.Equal(t, "gamma", d.Parsed.ToolName)
				require.NotEmpty(t, d.Trace, "Trace should be non-empty")
				assert.Equal(t, "tool", d.Trace[0].Path)
				assert.Equal(t, validator.StatusFail, d.Trace[0].Status)
			},
		},
		{
			name: "tool phase: name in allowlist, no args, allows remaining phases",
			spec: func(*testing.T) *mcpspec.Spec { return minimalSpec() },
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.True(t, d.Allow, "expected Allow=true; reason=%q failedOn=%+v", d.Reason, d.FailedOn)
				require.NotEmpty(t, d.Trace, "Trace should be non-empty")
				assert.Equal(t, "tool", d.Trace[0].Path)
				assert.Equal(t, validator.StatusPass, d.Trace[0].Status)
			},
		},
		{
			name: "allowedFields: empty allowlist + args present denies (fail closed)",
			spec: func(*testing.T) *mcpspec.Spec { return minimalSpec() }, // alpha has no AllowedFields
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{"anything": "x"},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.False(t, d.Allow, "an empty allowlist must NOT be allow-all")
				require.NotNil(t, d.FailedOn)
				assert.Equal(t, "allowedFields", d.FailedOn.Path)
				assert.Contains(t, d.Reason, "no allowedFields configured", "reason should explain the fail-closed default")
			},
		},
		{
			name: "allowedFields: empty allowlist + no args allows (arg-less tool)",
			spec: func(*testing.T) *mcpspec.Spec { return minimalSpec() },
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.True(t, d.Allow, "an arg-less call has nothing to reject; reason=%q", d.Reason)
			},
		},
		{
			name: "allowedFields: unconstrainedArgs opt-out allows arbitrary args",
			spec: func(*testing.T) *mcpspec.Spec {
				sp := minimalSpec()
				sp.Tools[0].Args = mcpspec.Args{UnconstrainedArgs: true}
				return sp
			},
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{"free": "form", "any": "key"},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.True(t, d.Allow, "unconstrainedArgs must permit free-form args; reason=%q", d.Reason)
			},
		},
		{
			name: "allowedFields: extra key denies with path=allowedFields",
			spec: specWithAllowedFields,
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{"foo": "x", "baz": "y"},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.False(t, d.Allow, "expected Allow=false for extra key")
				require.NotNil(t, d.FailedOn)
				assert.Equal(t, "allowedFields", d.FailedOn.Path)
				assert.Contains(t, d.Reason, "baz", "Reason should mention the extra key")
			},
		},
		{
			name: "allowedFields: subset of allowed keys passes",
			spec: specWithAllowedFields,
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{"foo": "x"},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.True(t, d.Allow, "expected Allow=true; reason=%q", d.Reason)
			},
		},
		{
			name: "constraints: false CEL denies with path=constraints[0] and message in reason",
			spec: func(t *testing.T) *mcpspec.Spec {
				return specWithConstraint(t, `args.repo == "allowed/repo"`, "wrong repo")
			},
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{"repo": "other/thing"},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.False(t, d.Allow, "expected Allow=false")
				require.NotNil(t, d.FailedOn)
				assert.Equal(t, "constraints[0]", d.FailedOn.Path)
				assert.Contains(t, d.Reason, "wrong repo", "Reason should carry constraint message")
			},
		},
		{
			name: "constraints: true CEL allows",
			spec: func(t *testing.T) *mcpspec.Spec {
				return specWithConstraint(t, `args.repo == "allowed/repo"`, "wrong repo")
			},
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{"repo": "allowed/repo"},
			},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.True(t, d.Allow, "expected Allow=true; reason=%q", d.Reason)
			},
		},
		{
			name: "constraints: malformed CEL returns internal error",
			spec: func(t *testing.T) *mcpspec.Spec {
				return specWithConstraint(t, `args.nope ( bogus`, "")
			},
			invocation: validator.Invocation{
				ToolName: "alpha",
				Args:     map[string]any{},
			},
			check: func(t *testing.T, _ *validator.Decision, err error) {
				require.Error(t, err, "expected internal error for malformed CEL")
			},
		},
		{
			name: "deny.effects.destructive: matched effect denies with that path",
			spec: func(*testing.T) *mcpspec.Spec {
				sp := minimalSpec()
				sp.Tools[0].Effects = mcpspec.Effects{Destructive: true}
				sp.Tools[0].Deny.Effects.Destructive = true
				return sp
			},
			invocation: validator.Invocation{ToolName: "alpha", Args: map[string]any{}},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.False(t, d.Allow, "expected Allow=false")
				require.NotNil(t, d.FailedOn)
				assert.Equal(t, "deny.effects.destructive", d.FailedOn.Path)
			},
		},
		{
			name: "deny.effects.destructive: unmatched effect allows",
			spec: func(*testing.T) *mcpspec.Spec {
				sp := minimalSpec()
				sp.Tools[0].Effects = mcpspec.Effects{Destructive: false}
				sp.Tools[0].Deny.Effects.Destructive = true
				return sp
			},
			invocation: validator.Invocation{ToolName: "alpha", Args: map[string]any{}},
			check: func(t *testing.T, d *validator.Decision, err error) {
				require.NoError(t, err, "Check")
				assert.True(t, d.Allow, "expected Allow=true; reason=%q", d.Reason)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := validator.Check(tc.spec(t), tc.invocation)
			tc.check(t, d, err)
		})
	}
}

// TestCheck_DenyTrust exercises the deny.trust validation phase against
// SEP-1913 Trust capability snapshots. Each axis denies only when both
// the static Trust declaration is present AND the spec opts into the
// corresponding deny flag — same && rule as deny.effects.
func TestCheck_DenyTrust(t *testing.T) {
	mkSpec := func(trust mcpspec.Trust, deny mcpspec.DenyTrust) *mcpspec.Spec {
		return &mcpspec.Spec{
			Name:    "s",
			Version: "1",
			Server: mcpspec.Server{
				URL:       "https://example.com/mcp",
				Transport: mcpspec.TransportStreamableHTTP,
			},
			Tools: []mcpspec.Tool{{
				Name:    "t",
				Args:    mcpspec.Args{AllowedFields: []string{}},
				Effects: mcpspec.Effects{},
				Trust:   trust,
				Deny:    mcpspec.Deny{Trust: deny},
			}},
		}
	}
	cases := []struct {
		name        string
		trust       mcpspec.Trust
		deny        mcpspec.DenyTrust
		wantAllow   bool
		wantPath    string
		wantWarning bool
	}{
		{
			name:      "no trust, no deny: allow",
			wantAllow: true,
		},
		{
			name:      "outcomesIrreversible HIT denies",
			trust:     mcpspec.Trust{InputMetadata: json.RawMessage(`{"outcomes":["irreversible"]}`)},
			deny:      mcpspec.DenyTrust{OutcomesIrreversible: true},
			wantAllow: false,
			wantPath:  "deny.trust.outcomesIrreversible",
		},
		{
			name:      "outcomesIrreversible MISS allows",
			trust:     mcpspec.Trust{InputMetadata: json.RawMessage(`{"outcomes":["benign"]}`)},
			deny:      mcpspec.DenyTrust{OutcomesIrreversible: true},
			wantAllow: true,
		},
		{
			name:      "outcomesIrreversible string form HIT denies",
			trust:     mcpspec.Trust{InputMetadata: json.RawMessage(`{"outcomes":"irreversible"}`)},
			deny:      mcpspec.DenyTrust{OutcomesIrreversible: true},
			wantAllow: false,
			wantPath:  "deny.trust.outcomesIrreversible",
		},
		{
			name:      "destinationPublic HIT denies",
			trust:     mcpspec.Trust{InputMetadata: json.RawMessage(`{"destination":"public"}`)},
			deny:      mcpspec.DenyTrust{DestinationPublic: true},
			wantAllow: false,
			wantPath:  "deny.trust.destinationPublic",
		},
		{
			name:      "sourceUntrustedPublic HIT denies",
			trust:     mcpspec.Trust{ReturnMetadata: json.RawMessage(`{"source":["untrustedPublic"]}`)},
			deny:      mcpspec.DenyTrust{SourceUntrustedPublic: true},
			wantAllow: false,
			wantPath:  "deny.trust.sourceUntrustedPublic",
		},
		{
			name:      "deny without matching trust capability allows",
			trust:     mcpspec.Trust{},
			deny:      mcpspec.DenyTrust{OutcomesIrreversible: true},
			wantAllow: true,
		},
		{
			name:      "empty inputMetadata allows",
			trust:     mcpspec.Trust{InputMetadata: json.RawMessage(`{}`)},
			deny:      mcpspec.DenyTrust{OutcomesIrreversible: true},
			wantAllow: true,
		},
		{
			name:        "malformed inputMetadata under active deny fails CLOSED",
			trust:       mcpspec.Trust{InputMetadata: json.RawMessage(`"not an object"`)},
			deny:        mcpspec.DenyTrust{OutcomesIrreversible: true},
			wantAllow:   false,
			wantPath:    "deny.trust.outcomesIrreversible",
			wantWarning: true,
		},
		{
			name:        "truncated returnMetadata under active deny fails CLOSED",
			trust:       mcpspec.Trust{ReturnMetadata: json.RawMessage(`{"source":`)},
			deny:        mcpspec.DenyTrust{SourceUntrustedPublic: true},
			wantAllow:   false,
			wantPath:    "deny.trust.sourceUntrustedPublic",
			wantWarning: true,
		},
		{
			name:      "malformed inputMetadata but deny NOT active allows",
			trust:     mcpspec.Trust{InputMetadata: json.RawMessage(`"not an object"`)},
			deny:      mcpspec.DenyTrust{},
			wantAllow: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp := mkSpec(c.trust, c.deny)
			d, err := validator.Check(sp, validator.Invocation{ToolName: "t"})
			require.NoError(t, err)
			assert.Equal(t, c.wantAllow, d.Allow, "decision allow")
			if !c.wantAllow {
				require.NotNil(t, d.FailedOn)
				assert.Equal(t, c.wantPath, d.FailedOn.Path)
			}
			if c.wantWarning {
				assert.NotEmpty(t, d.Warnings, "expected a warning describing the unparseable trust metadata")
			}
		})
	}
}
