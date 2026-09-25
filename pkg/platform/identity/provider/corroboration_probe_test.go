package provider

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aStderrPattern is a pattern that is fine on every OTHER axis the loader
// checks — it compiles, and it does not match the empty string — so a
// rejection can only come from the probe rule under test here, never from
// validateAuthFailureConfig's pre-existing rules.
const aStderrPattern = `(?m)^demo-cli: authentication failed`

// anExitCode is likewise clean under validateAuthFailureConfig: not 0 (which
// inverts the signal) and inside 1-255. Deliberately NOT 1 — 1 is the
// documented broad-code hazard, and using it here would blur a rejection by
// this rule with the separate review obligation that code carries.
const anExitCode = 42

func probeConfig() *VerifyConfig {
	return &VerifyConfig{Endpoint: "https://api.example.test/whoami"}
}

// TestValidateAgentShapedCorroborationHasProbe covers the derived rule:
// declaring an AGENT-SHAPED corroboration signal — stderrPatterns or exitCodes
// — without a verify: probe is refused at load.
//
// The combination is what makes it dangerous, which is why the rule is DERIVED
// from the fields rather than recorded as a hand-maintained sign-off flag. A
// flag would become a second source of truth the moment a verify: is added,
// and records only that someone typed `true` — no citation a later reviewer
// can check or refute.
//
// It covers BOTH agent-shaped fields on purpose. Covering one would be worse
// than covering either or neither: an author would hit the guard on one field
// and sail past it on the other, implying the uncovered one was considered
// safe. It is not — most CLIs exit 1 on any error the model's argv can cause.
func TestValidateAgentShapedCorroborationHasProbe(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		// wantFields are the field names the error must name; empty means the
		// provider must be accepted.
		wantFields []string
		// absentFields must NOT appear in the error — this is what keeps the
		// message specific to what actually tripped, instead of a boilerplate
		// paragraph naming every field the rule spans.
		absentFields []string
	}{
		{
			name: "stderrPatterns with no verify: REFUSED — corroboration would be the sole evidence",
			p: Provider{
				ID:          "no-probe-stderr",
				AuthFailure: &AuthFailure{StderrPatterns: []string{aStderrPattern}},
			},
			wantFields:   []string{"stderrPatterns"},
			absentFields: []string{"exitCodes"},
		},
		{
			name: "exitCodes with no verify: REFUSED — an exit code is argv-shaped too",
			p: Provider{
				ID:          "no-probe-exit",
				AuthFailure: &AuthFailure{ExitCodes: []int{anExitCode}},
			},
			wantFields:   []string{"exitCodes"},
			absentFields: []string{"stderrPatterns"},
		},
		{
			name: "both agent-shaped fields with no verify: REFUSED naming both",
			p: Provider{
				ID: "no-probe-both",
				AuthFailure: &AuthFailure{
					StderrPatterns: []string{aStderrPattern},
					ExitCodes:      []int{anExitCode},
				},
			},
			wantFields: []string{"stderrPatterns", "exitCodes"},
		},
		{
			name: "stderrPatterns WITH a verify: probe accepted — a match then corroborates rather than decides",
			p: Provider{
				ID:          "has-probe-stderr",
				Verify:      probeConfig(),
				AuthFailure: &AuthFailure{StderrPatterns: []string{aStderrPattern}},
			},
		},
		{
			name: "exitCodes WITH a verify: probe accepted",
			p: Provider{
				ID:          "has-probe-exit",
				Verify:      probeConfig(),
				AuthFailure: &AuthFailure{ExitCodes: []int{anExitCode}},
			},
		},
		{
			name: "neither agent-shaped field nor verify: accepted",
			p:    Provider{ID: "neither"},
		},

		// ZERO FALSE POSITIVES against the shapes the catalog actually ships.
		// oauth-mcp is the load-bearing one: httpStatuses-only corroboration
		// with no probe is the ONLY path to a credential card for an MCP
		// origin, and a rule that caught it would silently undo Task 1.
		{
			name: "httpStatuses-only with no verify: accepted (oauth-mcp's shape) — a 401 is not agent-forgeable",
			p: Provider{
				ID:          "http-only",
				AuthFailure: &AuthFailure{HTTPStatuses: []int{401}},
			},
		},
		{
			name: "empty authFailure block with no verify: accepted",
			p: Provider{
				ID:          "empty-block",
				AuthFailure: &AuthFailure{},
			},
		},
		{
			name: "verify: with no authFailure at all accepted",
			p:    Provider{ID: "probe-only", Verify: probeConfig()},
		},
		{
			name: "stderrPatterns alongside httpStatuses, still no verify: REFUSED — a sibling field is not a probe",
			p: Provider{
				ID: "mixed-stderr-no-probe",
				AuthFailure: &AuthFailure{
					HTTPStatuses:   []int{401},
					StderrPatterns: []string{aStderrPattern},
				},
			},
			wantFields:   []string{"stderrPatterns"},
			absentFields: []string{"exitCodes"},
		},
		{
			name: "exitCodes alongside httpStatuses, still no verify: REFUSED — a sibling field is not a probe",
			p: Provider{
				ID: "mixed-exit-no-probe",
				AuthFailure: &AuthFailure{
					HTTPStatuses: []int{401},
					ExitCodes:    []int{anExitCode},
				},
			},
			wantFields:   []string{"exitCodes"},
			absentFields: []string{"stderrPatterns"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentShapedCorroborationHasProbe(tc.p)
			if len(tc.wantFields) == 0 {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.p.ID, "error must name the provider")
			for _, f := range tc.wantFields {
				assert.Containsf(t, err.Error(), f, "error must name the offending field %q", f)
			}
			for _, f := range tc.absentFields {
				assert.NotContainsf(t, err.Error(), f,
					"error must not name %q, which this provider does not declare — a message that lists every "+
						"field the rule spans tells the author nothing about what they actually did", f)
			}
			assert.Contains(t, err.Error(), "verify:", "error must name the missing probe")
			// The error has to explain WHY the bar is higher without a probe
			// and what relaxing it costs — an author who only reads this line
			// must not conclude that adding an inert verify: is the fix.
			assert.Contains(t, err.Error(), "sole evidence",
				"error must say corroboration becomes the SOLE evidence without a probe")
			assert.Contains(t, err.Error(), "captured",
				"error must demand captured samples as evidence, not a sign-off")
			// This sentence is the ENTIRE mitigation for the accepted builtin:
			// KNOWN LIMIT (validate.go's doc comment above this function): a
			// declarative verify: block is a no-op on a builtin provider, and
			// nothing in this package can detect that, so the obligation to check
			// has to land in the text a reviewer actually reads. Trimming or
			// rewording this sentence would remove that mitigation silently, with
			// every other assertion here still green.
			assert.Contains(t, err.Error(), "builtin:",
				"error must carry the builtin: caveat — the only mitigation for the accepted KNOWN LIMIT")
		})
	}
}

// agentShapedArmings returns one arming per field this rule covers. Both
// positive-control tests below iterate it, so neither covered field can go
// unproven. Extending the rule to a third agent-shaped field without
// extending this table is not silent, either:
// TestAgentShapedArmingsCoverAllAuthFailureFields walks every AuthFailure
// field by reflection and fails until the new field is classified here or in
// that test's own safe-field list.
func agentShapedArmings() []struct {
	field string
	apply func(Provider) Provider
} {
	withAuthFailure := func(p Provider, mutate func(*AuthFailure)) Provider {
		armed := p
		var af AuthFailure
		if p.AuthFailure != nil {
			af = *p.AuthFailure
		}
		mutate(&af)
		armed.AuthFailure = &af
		return armed
	}
	return []struct {
		field string
		apply func(Provider) Provider
	}{
		{
			field: "stderrPatterns",
			apply: func(p Provider) Provider {
				return withAuthFailure(p, func(af *AuthFailure) { af.StderrPatterns = []string{aStderrPattern} })
			},
		},
		{
			field: "exitCodes",
			apply: func(p Provider) Provider {
				return withAuthFailure(p, func(af *AuthFailure) { af.ExitCodes = []int{anExitCode} })
			},
		},
	}
}

// TestAgentShapedArmingsCoverAllAuthFailureFields is what makes the "not
// silent" claim on agentShapedArmings' doc comment actually enforced rather
// than aspirational. It walks every exported field of AuthFailure by
// reflection and requires each one to be classified: either armed by
// agentShapedArmings() (agent-shaped, so it must be proven by the positive
// controls below) or listed in safeFields (deliberately excluded, the way
// HTTPStatuses is — see its doc comment for why). A field in neither list
// fails this test, so a new AuthFailure field cannot be added — and,
// specifically, a new `declared = append(declared, ...)` branch in
// validateAgentShapedCorroborationHasProbe cannot be wired up — without a
// deliberate classification decision landing in this file.
func TestAgentShapedArmingsCoverAllAuthFailureFields(t *testing.T) {
	// safeFields are AuthFailure fields deliberately excluded from the
	// agent-shaped rule. Adding an entry here is a security decision, not a
	// formality — it says "the agent cannot forge this field" — so keep it as
	// small and as justified as HTTPStatuses is on its own doc comment.
	safeFields := map[string]bool{
		"HTTPStatuses": true,
	}

	armedFields := map[string]bool{}
	for _, arm := range agentShapedArmings() {
		armedFields[arm.field] = true
	}

	typ := reflect.TypeOf(AuthFailure{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		yamlName := strings.SplitN(f.Tag.Get("yaml"), ",", 2)[0]
		require.NotEmptyf(t, yamlName, "AuthFailure.%s has no yaml tag; this test cannot classify it by name", f.Name)

		if safeFields[f.Name] {
			assert.Falsef(t, armedFields[yamlName],
				"AuthFailure.%s is listed in both safeFields and agentShapedArmings() — it cannot be both "+
					"forgeable and safe; pick one", f.Name)
			continue
		}
		assert.Truef(t, armedFields[yamlName],
			"AuthFailure.%s is classified as neither agent-shaped (agentShapedArmings()) nor safe (safeFields "+
				"above) — a new field must be classified explicitly, the same way stderrPatterns, exitCodes, and "+
				"httpStatuses already are", f.Name)
	}
}

// TestValidateAgentShapedCorroboration_AcceptanceIsNotVacuous is the POSITIVE
// CONTROL for the accepting cases above.
//
// "A provider with neither passes" is exactly the shape of proof that passes
// when the machinery never runs at all: a validator that unconditionally
// returned nil — or one the loader forgot to call — satisfies it. So for each
// accepted shape this asserts a ONE-FIELD DELTA, once per agent-shaped field:
// adding that field to the same provider, changing nothing else, must flip it
// to an error. That is only possible if the rule genuinely executed against
// that provider and genuinely can reject.
func TestValidateAgentShapedCorroboration_AcceptanceIsNotVacuous(t *testing.T) {
	accepted := []struct {
		name string
		p    Provider
	}{
		{name: "neither", p: Provider{ID: "control-neither"}},
		{name: "httpStatuses-only", p: Provider{ID: "control-http", AuthFailure: &AuthFailure{HTTPStatuses: []int{401}}}},
		{name: "empty block", p: Provider{ID: "control-empty", AuthFailure: &AuthFailure{}}},
	}

	for _, tc := range accepted {
		for _, arm := range agentShapedArmings() {
			t.Run(tc.name+": accepted, and arming it with "+arm.field+" flips it to an error", func(t *testing.T) {
				require.NoError(t, validateAgentShapedCorroborationHasProbe(tc.p),
					"precondition: this shape must be accepted")

				err := validateAgentShapedCorroborationHasProbe(arm.apply(tc.p))
				require.Errorf(t, err,
					"POSITIVE CONTROL FAILED: adding %s to an otherwise identical provider did not produce an "+
						"error, so the acceptance above proves nothing — the rule is inert for that field", arm.field)
				assert.Contains(t, err.Error(), arm.field)
			})
		}
	}
}

// TestShippedProvidersPassCorroborationProbeCheck is the zero-false-positives
// claim, asserted against the catalog this slice actually leaves behind rather
// than against hand-built fixtures.
//
// It carries its own positive control on the same real data, once per
// agent-shaped field: arming each shipped provider must flip it to an error
// exactly when that provider has no verify: probe. A rule that never fired
// would fail the arming half; a rule that fired indiscriminately would fail
// the first half.
func TestShippedProvidersPassCorroborationProbeCheck(t *testing.T) {
	all := All()
	require.NotEmpty(t, all, "no embedded providers loaded; every assertion below would be vacuous")

	for _, p := range all {
		t.Run(p.ID, func(t *testing.T) {
			assert.NoErrorf(t, validateAgentShapedCorroborationHasProbe(p),
				"shipped provider %q must pass unchanged — this rule must not break any existing catalog entry", p.ID)

			for _, arm := range agentShapedArmings() {
				t.Run("armed with "+arm.field, func(t *testing.T) {
					err := validateAgentShapedCorroborationHasProbe(arm.apply(p))
					if p.Verify == nil {
						require.Errorf(t, err,
							"POSITIVE CONTROL FAILED for %q: it declares no verify:, so arming it with %s must be "+
								"refused; that it was not means the pass above proves nothing", p.ID, arm.field)
						return
					}
					assert.NoErrorf(t, err,
						"provider %q declares a verify: probe, so %s is corroboration rather than sole evidence", p.ID, arm.field)
				})
			}
		})
	}
}

// TestLoaderRefusesAgentShapedCorroborationWithoutProbe proves the rule is
// WIRED INTO THE LOADER, not merely correct in isolation. Task 4 is "make the
// dangerous combination fail the build", and a predicate nobody calls fails
// nothing — this is the assertion that would have caught a forgotten call site.
//
// It drives the same loadFrom() the embedded catalog goes through, against a
// synthetic one-provider FS, so the failure it observes is the failure a
// catalog author would hit.
func TestLoaderRefusesAgentShapedCorroborationWithoutProbe(t *testing.T) {
	const stderrOnly = `
id: demo-cli-provider
shape: bearer
authFailure:
  stderrPatterns: ['(?m)^demo-cli: authentication failed']
`
	const exitCodesOnly = `
id: demo-cli-provider
shape: bearer
authFailure:
  exitCodes: [42]
`
	const probe = `
verify:
  endpoint: https://api.example.test/whoami
`

	cases := []struct {
		field string
		doc   string
		// check asserts the parsed provider really carries the field, so a
		// fixture that silently parsed to empty cannot masquerade as a pass.
		check func(t *testing.T, af *AuthFailure)
	}{
		{
			field: "stderrPatterns",
			doc:   stderrOnly,
			check: func(t *testing.T, af *AuthFailure) {
				t.Helper()
				assert.Equal(t, []string{aStderrPattern}, af.StderrPatterns,
					"the fixture must really carry the pattern; if it parsed to empty, the rejection above was about something else")
			},
		},
		{
			field: "exitCodes",
			doc:   exitCodesOnly,
			check: func(t *testing.T, af *AuthFailure) {
				t.Helper()
				assert.Equal(t, []int{anExitCode}, af.ExitCodes,
					"the fixture must really carry the exit code; if it parsed to empty, the rejection above was about something else")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.field+" and no verify: fails the load, naming the file and the provider", func(t *testing.T) {
			_, _, err := loadFrom(fstest.MapFS{"demo.yaml": &fstest.MapFile{Data: []byte(tc.doc)}})
			require.Error(t, err, "the loader must refuse this catalog")
			assert.Contains(t, err.Error(), "demo.yaml", "error must name the offending file")
			assert.Contains(t, err.Error(), "demo-cli-provider", "error must name the provider")
			assert.Contains(t, err.Error(), tc.field, "error must name the offending field")
			assert.Contains(t, err.Error(), "sole evidence", "error must explain why the bar is higher without a probe")
		})

		// POSITIVE CONTROL: the identical catalog plus a verify: block loads
		// cleanly. Without this, the failure above could equally be caused by
		// something unrelated in the fixture (a parse error, an empty id), and
		// the rejection would prove nothing about the rule.
		t.Run(tc.field+" WITH a verify: probe loads cleanly, and really carries the field", func(t *testing.T) {
			provs, byID, err := loadFrom(fstest.MapFS{"demo.yaml": &fstest.MapFile{Data: []byte(tc.doc + probe)}})
			require.NoError(t, err, "adding only a verify: block must make the same catalog loadable")
			require.Len(t, provs, 1)
			require.Contains(t, byID, "demo-cli-provider")
			require.NotNil(t, byID["demo-cli-provider"].AuthFailure)
			tc.check(t, byID["demo-cli-provider"].AuthFailure)
		})
	}
}
