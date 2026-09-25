// cmd/oap/internal/installcmd/pinning.go — ClusterAgentSettings pinning seed for
// `oap install --pinning-mode` / `oap init --pinning-mode`. Creates the
// singleton ClusterAgentSettings with one {kind, mode} rule per registered
// pinning kind. Create-only: an existing ClusterAgentSettings is never
// modified.
package installcmd

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

// pinningModeUsage is the shared flag description used by both `oap install`
// and `oap init` so the two help texts stay in sync.
const pinningModeUsage = "Seed the cluster-wide ClusterAgentSettings with a default dependency-pinning rule per registered kind, " +
	"set to this enforcement mode (block|approve|warn|off) — e.g. \"warn\" keeps pinning observable without gating, " +
	"useful for testing. Empty: seed nothing. " +
	"Create-only: an existing ClusterAgentSettings is never modified; only applies when installing."

// validPinningMode rejects values outside the PinningRule mode enum; "" (do
// not seed) is allowed.
func validPinningMode(mode string) error {
	switch mode {
	case "", v1alpha1.PinModeBlock, v1alpha1.PinModeApprove, v1alpha1.PinModeWarn, v1alpha1.PinModeOff:
		return nil
	}
	return fmt.Errorf("--pinning-mode must be one of block|approve|warn|off, got %q", mode)
}

// ensureClusterPinningMode seeds the singleton ClusterAgentSettings with one
// {kind, mode} pinning rule per registered pinning kind. Create-only: if the
// ClusterAgentSettings already exists (whatever its content), it is left
// untouched and a notice is printed — install must never clobber operator-
// managed settings.
//
// logf narrates each outcome through the install reporter (rep.Info) rather than
// writing to an io.Writer directly: this runs after the pipeline's Phase rows
// exist, so the checklist live region is up, and a raw write here would displace
// the cursor below the region and strand a partial rail copy on the next redraw.
// (audit2-tui NEW-2)
func ensureClusterPinningMode(ctx context.Context, logf func(string, ...any), c client.Client, mode string) error {
	if mode == "" {
		return nil
	}
	kinds := registry.All()
	rules := make([]v1alpha1.PinningRule, 0, len(kinds))
	for _, k := range kinds {
		rules = append(rules, v1alpha1.PinningRule{Kind: k.Name(), Mode: mode})
	}
	cas := &v1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		Spec: v1alpha1.SettingsSpec{Limits: &v1alpha1.SettingsLimits{
			Pinning: &v1alpha1.PinningPolicy{Rules: rules},
		}},
	}
	if err := c.Create(ctx, cas); err != nil {
		if apierrors.IsAlreadyExists(err) {
			logf("  ClusterAgentSettings %q already exists; leaving it untouched (--pinning-mode not applied)", v1alpha1.ClusterAgentSettingsName)
			return nil
		}
		return fmt.Errorf("create ClusterAgentSettings: %w", err)
	}
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.Name())
	}
	logf("  seeded ClusterAgentSettings %q with pinning mode %q for kinds [%s]",
		v1alpha1.ClusterAgentSettingsName, mode, strings.Join(names, ", "))
	return nil
}
