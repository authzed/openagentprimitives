package capability

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
)

func fakeClientForTest(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return fake.NewClientBuilder().WithScheme(s).Build()
}

func TestCredentialUpdateCapability_Registered(t *testing.T) {
	c, ok := Lookup("credential_update")
	require.True(t, ok, "importing this package must register credential_update")

	assert.False(t, c.DefaultOn(),
		"credential_update is opt-in: it can raise a credential-entry form, so an AgentClass must ask for it")
	assert.False(t, c.Infrastructural())
}

func TestCredentialUpdateCapability_Offer(t *testing.T) {
	c, ok := Lookup("credential_update")
	require.True(t, ok)

	t.Run("no client wired: contributes nothing and reports a skip rather than a silent no-op tool", func(t *testing.T) {
		tools, skip := c.Offer(OfferContext{Granted: true, Enabled: true})
		assert.Empty(t, tools)
		require.NotNil(t, skip, "a granted-but-unsatisfiable capability must report a SkipReason")
		assert.Equal(t, "credential_update", skip.Capability)
		assert.NotEmpty(t, skip.Reason)
	})

	t.Run("client wired, ToolLookup nil (not yet late-bound): still contributes exactly request_credential_update", func(t *testing.T) {
		// A zero-value RunnerEnv.ToolLookup is exactly what Offer sees at
		// capability.Assemble time in production too (see RunnerEnv.ToolLookup's
		// doc) — the capability must tolerate it, not skip on it.
		tools, skip := c.Offer(OfferContext{
			Granted: true, Enabled: true,
			Env: RunnerEnv{CredentialUpdateClient: fakeClientForTest(t)},
		})
		assert.Nil(t, skip)
		require.Len(t, tools, 1)
		assert.Equal(t, "request_credential_update", tools[0].Name())
	})
}

// TestCredentialUpdateCapability_AssembledFromARealAgentClassGrant is the
// wire-together the rest of this file's coverage stops short of. Every other
// test here calls Offer DIRECTLY with a hand-built OfferContext, which means
// the two things that decide whether an agent actually gets this tool -- the
// AgentClass's own capabilities stanza, and Assemble's granted/enabled gate on
// an opt-in capability -- are never exercised together with it. A capability
// that parses its config perfectly and offers the right tool is still absent
// from the agent's tool table if the grant plumbing between them is wrong.
//
// It runs against the REAL registry (no resetRegistryForTest), so what it
// asserts is the merged tool list a runner would genuinely build.
//
// Two directions matter equally. Deleting the credential_update key from the
// class must make the tool DISAPPEAR (an opt-in capability that leaks in by
// default would put a credential-entry form behind an agent nobody granted it
// to), and adding it back must make it APPEAR.
func TestCredentialUpdateCapability_AssembledFromARealAgentClassGrant(t *testing.T) {
	cases := []struct {
		name string
		spec spiceboxv1alpha1.AgentClassSpec
		want bool
	}{
		{
			name: "no capabilities stanza: opt-in capability contributes nothing",
			spec: spiceboxv1alpha1.AgentClassSpec{},
			want: false,
		},
		{
			name: "a DIFFERENT capability granted: credential_update still absent",
			spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("memory", `{}`)},
			want: false,
		},
		{
			name: "granted with a bare {}: request_credential_update is in the tool list",
			spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("credential_update", `{}`)},
			want: true,
		},
		{
			name: "granted with enabled=true: request_credential_update is in the tool list",
			spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("credential_update", `{"enabled":true}`)},
			want: true,
		},
		{
			name: "granted but enabled=false: explicitly switched off, so absent",
			spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("credential_update", `{"enabled":false}`)},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Assemble(context.Background(), AssembleDeps{
				Class:  &spiceboxv1alpha1.AgentClass{Spec: tc.spec},
				Env:    RunnerEnv{CredentialUpdateClient: fakeClientForTest(t)},
				Logger: logr.Discard(),
			})
			assert.Equal(t, tc.want, hasToolNamed(got, "request_credential_update"),
				"request_credential_update present in the assembled tool list")
		})
	}
}

// TestCredentialUpdateCapability_GrantedButUnwiredIsAbsentNotBroken pins the
// other half of the grant path: a class that ASKS for credential_update on a
// runner with no cluster client must end up with no such tool, rather than one
// that fails on first use. Assemble drops a skipped capability's tools
// entirely, and the skip is logged -- never a silent, half-wired tool.
func TestCredentialUpdateCapability_GrantedButUnwiredIsAbsentNotBroken(t *testing.T) {
	got := Assemble(context.Background(), AssembleDeps{
		Class:  &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps("credential_update", `{}`)}},
		Env:    RunnerEnv{}, // no CredentialUpdateClient
		Logger: logr.Discard(),
	})
	assert.False(t, hasToolNamed(got, "request_credential_update"),
		"a granted-but-unsatisfiable capability must contribute nothing, not a tool that cannot write")
}

func hasToolNamed(tools []tool.Tool, name string) bool {
	for _, tt := range tools {
		if tt.Name() == name {
			return true
		}
	}
	return false
}

func TestCredentialUpdateCapability_ParseConfig(t *testing.T) {
	c, ok := Lookup("credential_update")
	require.True(t, ok)

	t.Run("unknown key is rejected: a typo in the AgentClass must fail loudly, not be ignored", func(t *testing.T) {
		_, err := c.ParseConfig(json.RawMessage(`{"bogus": true}`))
		assert.Error(t, err)
	})

	t.Run("empty raw is accepted", func(t *testing.T) {
		_, err := c.ParseConfig(json.RawMessage(``))
		assert.NoError(t, err)
	})

	t.Run("the common enabled flag alone is accepted, not mistaken for an unknown key", func(t *testing.T) {
		_, err := c.ParseConfig(json.RawMessage(`{"enabled": false}`))
		assert.NoError(t, err, "the shared {enabled} blob must not be rejected as an unsupported key")
	})
}

func TestCredentialUpdateMaxWaitFloor(t *testing.T) {
	cases := []struct {
		name    string
		idleTTL time.Duration
		want    time.Duration
	}{
		{
			name:    "idle disabled (zero) floors to the 10m default, not 0",
			idleTTL: 0,
			want:    credentialUpdateMinWait,
		},
		{
			name:    "an IdleTTL shorter than the floor is raised to the floor",
			idleTTL: 30 * time.Second,
			want:    credentialUpdateMinWait,
		},
		{
			name:    "an IdleTTL exactly at the floor passes through unchanged",
			idleTTL: credentialUpdateMinWait,
			want:    credentialUpdateMinWait,
		},
		{
			name:    "an IdleTTL longer than the floor passes through unchanged",
			idleTTL: 20 * time.Minute,
			want:    20 * time.Minute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, credentialUpdateMaxWait(tc.idleTTL))
		})
	}
}

// TestCredentialUpdateMinWaitDerivesFromSharedBase pins this package's half of
// the cross-binary timing contract in pkg/platform/identity/credupdate/timing.go: the
// tool's wait floor is an ALIAS of the shared DefaultToolWait, not a restated
// literal that happens to agree today. The `const _ = uint64(...)` guard next
// to the constant already fails the BUILD on an ordering-breaking edit; this
// asserts the alias itself, which is what
// TestCredentialUpdateTimingOrderingHoldsAcrossAllThreeBinaries (in
// credupdate_test, where this unexported constant is not visible) has to take
// on faith.
func TestCredentialUpdateMinWaitDerivesFromSharedBase(t *testing.T) {
	assert.Equal(t, credupdate.DefaultToolWait, credentialUpdateMinWait,
		"the tool's wait floor must alias the shared base, not restate its value")
	assert.LessOrEqual(t, credentialUpdateMinWait, credupdate.DefaultAskWindow,
		"tool wait <= park TTL: the tool must give up no later than the operator expires the request")
}
