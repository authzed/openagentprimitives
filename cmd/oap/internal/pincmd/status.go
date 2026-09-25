package pincmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
)

// kindFilterName returns the canonical kind name that a given --kind flag value
// maps to, or "" when kindFlag is empty (meaning "all"). The flag value is
// compared case-insensitively. "skill" matches both "skill" and "clusterskill"
// rows (both resource types carry skill pins).
//
// Valid flag values are the names registered in the pinning registry plus the
// special alias "skill" which covers Skill + ClusterSkill rows.
func kindFilterName(kindFlag string) (string, error) {
	if kindFlag == "" {
		return "", nil
	}
	kf := strings.ToLower(kindFlag)

	// Collect valid names from the registry for the error message.
	all := registry.All()
	validNames := make([]string, 0, len(all))
	for _, k := range all {
		validNames = append(validNames, k.Name())
	}

	for _, k := range all {
		if k.Name() == kf {
			return kf, nil
		}
	}
	return "", fmt.Errorf("unknown --kind %q; valid kinds: %s", kindFlag, strings.Join(validNames, ", "))
}

// kindMatches returns true when the row's pinning kind matches the filter.
// An empty filter matches everything. "skill" matches both "skill" entries
// (Skill and ClusterSkill resource types share the same pin kind name).
func kindMatches(filter, rowKind string) bool {
	if filter == "" {
		return true
	}
	return rowKind == filter
}

func newPinStatusCmd(g *apcmd.Globals) *cobra.Command {
	var kindFlag string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "List all pinned dependencies with strength, digest, drift, and age",
		RunE: func(cmd *cobra.Command, args []string) error {
			filter, err := kindFilterName(kindFlag)
			if err != nil {
				return err
			}

			b, err := g.Bundle()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			eff, err := fetchEffectivePinning(ctx, b.Controller, b.Namespace)
			if err != nil {
				return err
			}

			t := g.Table(out, "KIND", "NAME", "STRENGTH", "DIGEST", "VERSION", "DRIFT", "MODE", "OBSERVED")

			// Namespaced: Skills
			if kindMatches(filter, "skill") {
				var skills spiceboxv1alpha1.SkillList
				if err := b.Controller.List(ctx, &skills, client.InNamespace(b.Namespace)); err != nil {
					return fmt.Errorf("list Skills in %q: %w", b.Namespace, err)
				}
				for i := range skills.Items {
					s := &skills.Items[i]
					// Bypass patterns match the canonical name, not the
					// RFC1123 metadata.name slug.
					mode := effectiveModeFor(eff, "skill", s.Spec.CanonicalName)
					pinStatusRow(t, "Skill", s.Name, s.Status.Pin, s.Status.Conditions, mode)
				}
			}

			// Namespaced: MCPServers
			if kindMatches(filter, "mcp") {
				var mcpServers spiceboxv1alpha1.MCPServerList
				if err := b.Controller.List(ctx, &mcpServers, client.InNamespace(b.Namespace)); err != nil {
					return fmt.Errorf("list MCPServers in %q: %w", b.Namespace, err)
				}
				for i := range mcpServers.Items {
					s := &mcpServers.Items[i]
					mode := effectiveModeFor(eff, "mcp", s.Name)
					pinStatusRow(t, "MCPServer", s.Name, s.Status.Pin, s.Status.Conditions, mode)
				}
			}

			// Namespaced: SidecarToolboxes
			if kindMatches(filter, "image") {
				var sidecars spiceboxv1alpha1.SidecarToolboxList
				if err := b.Controller.List(ctx, &sidecars, client.InNamespace(b.Namespace)); err != nil {
					return fmt.Errorf("list SidecarToolboxes in %q: %w", b.Namespace, err)
				}
				for i := range sidecars.Items {
					s := &sidecars.Items[i]
					mode := effectiveModeFor(eff, "image", s.Name)
					pinStatusRow(t, "SidecarToolbox", s.Name, s.Status.Pin, s.Status.Conditions, mode)
				}
			}

			// Cluster-scoped: ClusterSkills
			if kindMatches(filter, "skill") {
				var clusterSkills spiceboxv1alpha1.ClusterSkillList
				if err := b.Controller.List(ctx, &clusterSkills); err != nil {
					return fmt.Errorf("list ClusterSkills: %w", err)
				}
				for i := range clusterSkills.Items {
					s := &clusterSkills.Items[i]
					mode := effectiveModeFor(eff, "skill", s.Spec.CanonicalName)
					pinStatusRow(t, "ClusterSkill", s.Name, s.Status.Pin, s.Status.Conditions, mode)
				}
			}

			// Cluster-scoped: SpiceboxToolkits
			if kindMatches(filter, "cli") {
				var toolkits spiceboxv1alpha1.SpiceboxToolkitList
				if err := b.Controller.List(ctx, &toolkits); err != nil {
					return fmt.Errorf("list SpiceboxToolkits: %w", err)
				}
				for i := range toolkits.Items {
					s := &toolkits.Items[i]
					mode := effectiveModeFor(eff, "cli", s.Name)
					pinStatusRow(t, "SpiceboxToolkit", s.Name, s.Status.Pin, s.Status.Conditions, mode)
				}
			}

			if t.Len() == 0 {
				fmt.Fprintln(out, "no pinned dependencies found")
				return nil
			}
			_, err = fmt.Fprint(out, t.Render())
			return err
		},
	}
	cmd.Flags().StringVar(&kindFlag, "kind", "", "Filter rows to one kind (skill, mcp, image, cli); skill covers both Skill and ClusterSkill rows")
	return cmd
}

// fetchEffectivePinning loads the two settings-tier singletons (cluster +
// namespace) and folds their pinning policies. A missing singleton is a nil
// tier (settings are optional); any other read error surfaces. Returns nil
// when neither tier sets pinning — the MODE column then renders "-".
func fetchEffectivePinning(ctx context.Context, c client.Client, namespace string) (*spiceboxv1alpha1.EffectivePinning, error) {
	var clusterSpec, nsSpec *spiceboxv1alpha1.SettingsSpec

	var cas spiceboxv1alpha1.ClusterAgentSettings
	if err := c.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cas); err == nil {
		clusterSpec = &cas.Spec
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get ClusterAgentSettings %q: %w", spiceboxv1alpha1.ClusterAgentSettingsName, err)
	}

	var as spiceboxv1alpha1.AgentSettings
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: spiceboxv1alpha1.AgentSettingsName}, &as); err == nil {
		nsSpec = &as.Spec
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get AgentSettings %s/%s: %w", namespace, spiceboxv1alpha1.AgentSettingsName, err)
	}

	return settings.EffectivePinningFor(clusterSpec, nsSpec), nil
}

// effectiveModeFor renders the MODE column for one dependency: the
// enforcement mode the resolved settings tiers would apply (block, approve,
// warn, off), "bypassed" when a bypass shielded every applicable rule, or
// "-" when no rule applies at all. policyName is the name bypass patterns
// match against — the canonical name for skills, the CR name otherwise.
func effectiveModeFor(eff *spiceboxv1alpha1.EffectivePinning, kind, policyName string) string {
	if eff == nil {
		return "-"
	}
	req := settings.PinRequirementFor(eff.Cluster, eff.Namespace, kind, policyName)
	switch {
	case req.HasRule():
		return req.Mode
	case req.BypassReason != "":
		return "bypassed"
	default:
		return "-"
	}
}

// pinStatusRow appends one row for a single object. pin may be nil when the
// reconciler hasn't observed an identity yet. mode is the pre-computed
// effective enforcement mode ("-" when no rule applies).
func pinStatusRow(t *tui.Table, kind, name string, pin *spiceboxv1alpha1.PinRecord, conds []metav1.Condition, mode string) {
	strength, digest, version, observed := "-", "-", "-", "-"
	if pin != nil {
		strength = pin.Strength
		if pin.Digest != "" {
			digest = truncateDigest(pin.Digest)
		}
		if pin.Version != "" {
			version = pin.Version
		}
		if pin.ObservedAt != nil && !pin.ObservedAt.IsZero() {
			observed = humanizeTime(pin.ObservedAt.Time)
		}
	}

	t.Row(kind, name, strength, digest, version, pinDriftReason(conds), mode, observed)
}

// truncateDigest shortens a digest to 19 chars: the "sha256:" prefix (7) plus
// 12 hex chars, matching the plan's display spec.
func truncateDigest(d string) string {
	const prefix = "sha256:"
	if len(d) <= 19 {
		return d
	}
	if len(d) > 7 && d[:7] == prefix {
		return d[:19]
	}
	// Non-sha256 digest: truncate at 19.
	return d[:19]
}

// humanizeTime renders a time as a human-readable relative age using
// go-humanize, e.g. "3 minutes ago", "2 hours ago".
func humanizeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return humanize.Time(t)
}

// pinDriftReason extracts the PinDrift condition reason from a conditions slice.
// Returns "-" when the condition is absent.
func pinDriftReason(conds []metav1.Condition) string {
	c := meta.FindStatusCondition(conds, spiceboxv1alpha1.PinDriftCondition)
	if c == nil {
		return "-"
	}
	return c.Reason
}
