package installcmd

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
)

// captureLogf returns a logf that appends each narration line to buf, standing
// in for the install reporter's rep.Info that RunInstall now threads into
// ensureClusterPinningMode. Asserting on buf proves the seed's narration flows
// through the reporter rather than a raw io.Writer — the fix for the live-region
// corruption (audit2-tui NEW-2).
func captureLogf(buf *bytes.Buffer) func(string, ...any) {
	return func(f string, a ...any) { fmt.Fprintf(buf, f+"\n", a...) }
}

func TestValidPinningMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		wantErr bool
		errMsg  string
	}{
		{name: "empty is valid (do not seed)", mode: "", wantErr: false},
		{name: "block is valid", mode: v1alpha1.PinModeBlock, wantErr: false},
		{name: "approve is valid", mode: v1alpha1.PinModeApprove, wantErr: false},
		{name: "warn is valid", mode: v1alpha1.PinModeWarn, wantErr: false},
		{name: "off is valid", mode: v1alpha1.PinModeOff, wantErr: false},
		{name: `invalid value: error mentions enum`, mode: "strict", wantErr: true, errMsg: `--pinning-mode must be one of block|approve|warn|off, got "strict"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validPinningMode(tc.mode)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestEnsureClusterPinningMode_SeedsRules(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	var out bytes.Buffer

	require.NoError(t, ensureClusterPinningMode(ctx, captureLogf(&out), c, v1alpha1.PinModeWarn))

	var cas v1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: v1alpha1.ClusterAgentSettingsName}, &cas),
		"ClusterAgentSettings must exist after seeding")

	require.NotNil(t, cas.Spec.Limits, "Limits must be set")
	require.NotNil(t, cas.Spec.Limits.Pinning, "Pinning must be set")
	require.NotEmpty(t, cas.Spec.Limits.Pinning.Rules, "at least one rule must be present")

	// The skill kind is registered via the blank import; find its rule.
	var skillRule *v1alpha1.PinningRule
	for i := range cas.Spec.Limits.Pinning.Rules {
		if cas.Spec.Limits.Pinning.Rules[i].Kind == "skill" {
			skillRule = &cas.Spec.Limits.Pinning.Rules[i]
			break
		}
	}
	require.NotNil(t, skillRule, "a rule for kind=skill must be present")
	assert.Equal(t, v1alpha1.PinModeWarn, skillRule.Mode, "skill rule must have mode=warn")

	// All rules must carry the requested mode.
	for _, r := range cas.Spec.Limits.Pinning.Rules {
		assert.Equal(t, v1alpha1.PinModeWarn, r.Mode, "rule %q must have mode=warn", r.Kind)
	}

	assert.Contains(t, out.String(), v1alpha1.ClusterAgentSettingsName,
		"output must mention the settings name")
	assert.Contains(t, out.String(), v1alpha1.PinModeWarn, "output must mention the mode")
}

func TestEnsureClusterPinningMode_EmptyModeNoop(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	var out bytes.Buffer

	require.NoError(t, ensureClusterPinningMode(ctx, captureLogf(&out), c, ""))

	var list v1alpha1.ClusterAgentSettingsList
	require.NoError(t, c.List(ctx, &list))
	assert.Empty(t, list.Items, "empty mode must not create any ClusterAgentSettings")
	assert.Empty(t, out.String(), "empty mode must produce no output")
}

func TestEnsureClusterPinningMode_ExistingUntouched(t *testing.T) {
	ctx := context.Background()

	// Pre-create the singleton with different content.
	existing := &v1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		Spec: v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{
			Pinning: &v1alpha1.PinningPolicy{
				Rules: []v1alpha1.PinningRule{{Kind: "skill", Mode: v1alpha1.PinModeBlock}},
			},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(existing).Build()
	var out bytes.Buffer

	require.NoError(t, ensureClusterPinningMode(ctx, captureLogf(&out), c, v1alpha1.PinModeWarn),
		"call must succeed even when ClusterAgentSettings already exists")

	// Fetch and confirm the content was not modified.
	var got v1alpha1.ClusterAgentSettings
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: v1alpha1.ClusterAgentSettingsName}, &got))
	require.Len(t, got.Spec.Limits.Pinning.Rules, 1, "existing rules must be unchanged")
	assert.Equal(t, v1alpha1.PinModeBlock, got.Spec.Limits.Pinning.Rules[0].Mode,
		"existing mode must not be overwritten")

	assert.Contains(t, out.String(), "already exists",
		"output must note that the existing resource was left untouched")
}
