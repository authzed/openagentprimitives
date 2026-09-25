package v1alpha1_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests/crdschematest"
)

func TestAgentClassAuthzBlock_RoundTrip(t *testing.T) {
	ten := metav1.Duration{Duration: 10 * time.Minute}
	ac := AgentClass{
		Spec: AgentClassSpec{
			Authz: &AuthzBlock{
				ApprovalTimeout: &ten,
				ToolCalls: &ToolCallsAuthz{
					Subject: "currentRequester",
					Mode:    "enforcing",
				},
				Session: &SessionAuthz{InteractPermission: "user:owner"},
			},
		},
	}
	buf, err := json.Marshal(ac)
	require.NoError(t, err)

	var back AgentClass
	require.NoError(t, json.Unmarshal(buf, &back))

	require.NotNil(t, back.Spec.Authz)
	require.NotNil(t, back.Spec.Authz.ToolCalls)
	require.NotNil(t, back.Spec.Authz.Session)
	require.NotNil(t, back.Spec.Authz.ApprovalTimeout)

	assert.Equal(t, "currentRequester", back.Spec.Authz.ToolCalls.Subject)
	assert.Equal(t, "enforcing", back.Spec.Authz.ToolCalls.Mode)
	assert.Equal(t, ten, *back.Spec.Authz.ApprovalTimeout)
	assert.Equal(t, "user:owner", back.Spec.Authz.Session.InteractPermission)
}

func TestInformationLeakagePolicy_RoundTrip(t *testing.T) {
	ten := metav1.Duration{Duration: 10 * time.Minute}
	boolTrue := true
	p := &InformationLeakagePolicy{
		Mode:                     "enforcing",
		ApprovalTTL:              &ten,
		OnUnsupportedChannel:     "blockBinding",
		SingleUserBypass:         &boolTrue,
		LoggingNoticeToRequester: &boolTrue,
	}
	buf, err := json.Marshal(p)
	require.NoError(t, err)

	var back InformationLeakagePolicy
	require.NoError(t, json.Unmarshal(buf, &back))

	assert.Equal(t, "enforcing", back.Mode)
	require.NotNil(t, back.ApprovalTTL)
	assert.Equal(t, ten, *back.ApprovalTTL)
	assert.Equal(t, "blockBinding", back.OnUnsupportedChannel)
	require.NotNil(t, back.SingleUserBypass)
	assert.True(t, *back.SingleUserBypass)
	require.NotNil(t, back.LoggingNoticeToRequester)
	assert.True(t, *back.LoggingNoticeToRequester)
}

func TestInformationLeakagePolicy_Resolved_NilPolicy(t *testing.T) {
	var p *InformationLeakagePolicy
	assert.Equal(t, "disabled", p.ResolvedMode())
	assert.Equal(t, 10*time.Minute, p.ResolvedApprovalTTL())
	assert.Equal(t, "blockBinding", p.ResolvedOnUnsupportedChannel())
	assert.True(t, p.ResolvedSingleUserBypass())
	assert.False(t, p.ResolvedLoggingNoticeToRequester()) // opt-in: default off
}

func TestAuthzBlock_ResolvedApprovalTimeout(t *testing.T) {
	t.Run("nil block defaults to 10m", func(t *testing.T) {
		var b *AuthzBlock
		assert.Equal(t, 10*time.Minute, b.ResolvedApprovalTimeout())
	})
	t.Run("block with nil timeout defaults to 10m", func(t *testing.T) {
		b := &AuthzBlock{}
		assert.Equal(t, 10*time.Minute, b.ResolvedApprovalTimeout())
	})
	t.Run("set value is returned", func(t *testing.T) {
		two := metav1.Duration{Duration: 2 * time.Minute}
		b := &AuthzBlock{ApprovalTimeout: &two}
		assert.Equal(t, 2*time.Minute, b.ResolvedApprovalTimeout())
	})
}

func TestInformationLeakagePolicy_Resolved_PartialOverrides(t *testing.T) {
	boolFalse := false
	p := &InformationLeakagePolicy{
		Mode:                     "logging",
		SingleUserBypass:         &boolFalse,
		LoggingNoticeToRequester: &boolFalse,
	}
	assert.Equal(t, "logging", p.ResolvedMode())
	assert.Equal(t, 10*time.Minute, p.ResolvedApprovalTTL())          // default
	assert.Equal(t, "blockBinding", p.ResolvedOnUnsupportedChannel()) // default
	assert.False(t, p.ResolvedSingleUserBypass())                     // override
	assert.False(t, p.ResolvedLoggingNoticeToRequester())             // override
}

func TestAgentClassCarriesTheDeploymentGrant(t *testing.T) {
	in := AgentClass{
		Spec: AgentClassSpec{
			AgentUI: &AgentClassUIGrant{
				Ref:          "leads-console",
				GrantedTools: []string{"list_leads"},
			},
		},
	}
	raw, err := json.Marshal(in)
	require.NoError(t, err)

	// A struct round-trip (marshal then unmarshal into the SAME type) can't
	// catch a wrong json tag: both sides read the same tag, so a typo'd
	// "agentUi" would round-trip clean while still breaking every real
	// consumer (kubectl, the CRD schema, another language's client). Assert
	// the literal wire keys too.
	assert.Contains(t, string(raw), `"agentUI":`)
	assert.Contains(t, string(raw), `"ref":"leads-console"`)
	assert.Contains(t, string(raw), `"grantedTools":["list_leads"]`)

	var out AgentClass
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Spec.AgentUI)
	assert.Equal(t, "leads-console", out.Spec.AgentUI.Ref)
	assert.Equal(t, []string{"list_leads"}, out.Spec.AgentUI.GrantedTools)
}

func TestAgentClassGrantAbsentMeansNoUICallableTools(t *testing.T) {
	var in AgentClass
	assert.Nil(t, in.Spec.AgentUI,
		"absent grant ⇒ no UI-callable tools; granting is an explicit deployment act")
}

// TestAgentSkill_TargetDefaultsToAgentInTheGeneratedCRD reads the default and
// enum out of the SHIPPED install bundle (like
// TestSpiceboxMount_FormatDefaultsToRawInTheGeneratedCRD in
// pkg/platform/podspec/builder_test.go), not off the Go marker it is
// supposed to be checking: deleting +kubebuilder:default=agent and
// regenerating would leave this test red, where a Go-side assertion of the
// marker's own text would stay green no matter what the CRD ends up saying.
func TestAgentSkill_TargetDefaultsToAgentInTheGeneratedCRD(t *testing.T) {
	schema := crdschematest.FieldSchema(t, "agentclasses", "spec.skills.items.properties.target")

	require.NotNil(t, schema.Default, "target field must carry a default in the generated CRD")
	assert.JSONEq(t, `"agent"`, string(schema.Default.Raw),
		"a skill with no target must behave exactly as it did before this field existed")

	enum := make([]string, 0, len(schema.Enum))
	for _, e := range schema.Enum {
		var v string
		require.NoError(t, json.Unmarshal(e.Raw, &v), "enum entry must decode to a string")
		enum = append(enum, v)
	}
	assert.ElementsMatch(t, []string{"agent", "sandbox", "both"}, enum)
}

// TestAgentSkill_DeepCopyDoesNotAliasEntries pins that AgentClassSpec.DeepCopy
// produces an independent Skills backing array: mutating the copy must never
// reach back into the original, the same hazard the generated
// DeepCopyInto/copy() pair for every other object-slice field guards against.
func TestAgentSkill_DeepCopyDoesNotAliasEntries(t *testing.T) {
	in := AgentClassSpec{Skills: []AgentSkill{
		{Name: "code-review", Ref: "github.com/demo-org/skills//skills/code-review@v1", Target: SkillTargetSandbox},
	}}
	out := in.DeepCopy()
	out.Skills[0].Name = "mutated"
	assert.Equal(t, "code-review", in.Skills[0].Name)
}

// TestSlotPrecondition_DeepCopyDoesNotAliasApprovers pins that an AuthzSlot
// deep-copied through its generated DeepCopyInto gives each SlotPrecondition an
// independent Approvers backing array. The field is a []string reached two
// levels down (AuthzSlot.Requires[].Approvers); until `mage gen:api` regenerates
// both DeepCopyInto methods, the shallow copies alias it and a mutation of the
// copy reaches back into the original — the same hazard the copy() pair guards
// for every other object-slice field.
func TestSlotPrecondition_DeepCopyDoesNotAliasApprovers(t *testing.T) {
	in := AuthzSlot{
		ResourceType: "git_commit",
		Permission:   "read",
		Requires: []SlotPrecondition{{
			CEL:              "facts.observed.is_cross_repository == false",
			UndeterminedHint: "call gitlike_gh to observe the pull request first",
			RefusalMessage:   "the head of this pull request lives on a fork",
			Approvers:        []string{"a", "b"},
		}},
	}
	out := in.DeepCopy()
	require.Len(t, out.Requires, 1)
	require.Equal(t, []string{"a", "b"}, out.Requires[0].Approvers)
	out.Requires[0].Approvers[0] = "mutated"
	assert.Equal(t, "a", in.Requires[0].Approvers[0],
		"the copy's Approvers must be a distinct backing array, not aliased to the original")
}

// TestModelProviderCRDEnum_IncludesOpenAI mirrors
// TestChannelCRD_KindEnum_IncludesLocal: it asserts the generated CRDs for
// every type carrying a Model.Provider field (ModelConfig, ModelCatalogEntry,
// DefaultModel) list "openai" as an accepted enum value after `mage gen:api`.
// This is the only place provider values are validated (no Go-side
// allowlist exists), so this test is the regression guard for Task 6.
func TestModelProviderCRDEnum_IncludesOpenAI(t *testing.T) {
	crdFiles := []string{
		"../../../config/crds/agentprimitives.authzed.com_agentclasses.yaml",
		"../../../config/crds/agentprimitives.authzed.com_agentsessions.yaml",
		"../../../config/crds/agentprimitives.authzed.com_agentsettings.yaml",
		"../../../config/crds/agentprimitives.authzed.com_clusteragentsettings.yaml",
	}
	for _, f := range crdFiles {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile(f)
			require.NoError(t, err, "generated CRD must exist")
			assert.Contains(t, string(data), "- openai",
				"the provider enum in %s must accept 'openai' after gen:api", f)
			assert.Contains(t, string(data), "- anthropic",
				"the provider enum in %s must still accept 'anthropic' (default unchanged)", f)
		})
	}
}

// A slot's permission set is Permission plus Permissions, deduplicated and in
// declaration order. Read through this rather than either field: the schema
// composer and the binding site must agree on what the slot covers, and a
// mismatch writes a grant against a relation the schema never emitted — which
// fails at SpiceDB with a FailedPrecondition nobody sees.
func TestAuthzSlot_EffectivePermissions(t *testing.T) {
	cases := []struct {
		name string
		slot AuthzSlot
		want []string
	}{
		{
			name: "singular only, the shape every existing class uses",
			slot: AuthzSlot{Permission: "push"},
			want: []string{"push"},
		},
		{
			name: "singular plus additional, in declaration order",
			slot: AuthzSlot{Permission: "push", Permissions: []string{"read", "write"}},
			want: []string{"push", "read", "write"},
		},
		{
			name: "a repeat of the singular is not listed twice",
			slot: AuthzSlot{Permission: "push", Permissions: []string{"push", "read"}},
			want: []string{"push", "read"},
		},
		{
			name: "empties are dropped rather than emitted as a blank permission",
			slot: AuthzSlot{Permission: "", Permissions: []string{"", "read"}},
			want: []string{"read"},
		},
		{
			name: "a slot naming nothing yields nothing",
			slot: AuthzSlot{},
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.slot.EffectivePermissions())
		})
	}
}
