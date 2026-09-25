package pincmd

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcppin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// refreezeIdentityRe matches a valid pin-refreeze identity: "sha256:" followed
// by exactly 64 lowercase hex characters.
var refreezeIdentityRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// detailKeyObservedDrift is the status.Pin.Details key used by both reconcilers
// to record the live identity when drift is detected, enabling --current accepts.
const detailKeyObservedDrift = "observedDriftDigest"

func newPinUpdateCmd(g *apcmd.Globals) *cobra.Command {
	var to string
	var current bool
	var all bool
	var kindFilter string

	cmd := &cobra.Command{
		Use:   "update <kind> <name>",
		Short: "Set the pin-refreeze annotation to accept a new baseline",
		Long: `Set the agentprimitives.authzed.com/pin-refreeze annotation on a dependency,
requesting that the reconciler accept the given identity as the new pin baseline.

The reconciler accepts the refreeze only when the live identity equals the annotated
value (race-free accept: you re-freeze exactly what you reviewed). Once honored, the
annotation is cleared and PinDrift is set to PinMatch.

Supported kinds:
  mcp    — probe tools/list unauthenticated to derive the live hash, or pass --to
  image  — --to <sha256:digest> required (CLI cannot pull images), or --current

Rejected kinds (identity is declared in spec, not set via annotation):
  skill  — edit spec.source.resolvedSHA or the SkillSource instead
  cli    — edit spec.pinnedBinaryHash or versionRange instead

Flags:
  --to <sha256:…>   explicit target identity (required for image without --current)
  --current         accept the observed drift digest recorded by the reconciler
  --all             accept all drifted resources in the namespace (no positional args)
  --kind mcp|image  with --all, restrict to one resource kind`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --all mode: no positional args; --to is rejected; --kind optional filter.
			if all {
				if len(args) > 0 {
					return fmt.Errorf("--all accepts no positional arguments; got: %v", args)
				}
				if to != "" {
					return fmt.Errorf("--to is per-resource; --all requires --current (identity comes from each resource's recorded drift)")
				}
				if !current {
					return fmt.Errorf("--all requires --current (identity is read from each resource's recorded drift)")
				}
				if kindFilter != "" {
					kf := strings.ToLower(kindFilter)
					if kf != "mcp" && kf != "image" {
						return fmt.Errorf("--kind must be one of: mcp, image (got %q)", kindFilter)
					}
					kindFilter = kf
				}
				b, err := g.Bundle()
				if err != nil {
					return err
				}
				return runPinUpdateAll(cmd.Context(), cmd, g.Theme(cmd.OutOrStdout()), b, kindFilter)
			}

			// Single-resource mode requires exactly 2 args.
			if len(args) != 2 {
				return fmt.Errorf("update requires <kind> <name>, or use --all to accept all drifted resources")
			}
			kindArg := strings.ToLower(args[0])
			name := args[1]

			// --to and --current are mutually exclusive.
			if to != "" && current {
				return fmt.Errorf("--to and --current are mutually exclusive; use one or the other")
			}

			// Validate kind and flags before Bundle() so usage errors need no kubeconfig.
			switch kindArg {
			case "mcp":
				if to != "" && !refreezeIdentityRe.MatchString(to) {
					return fmt.Errorf(`--to must be "sha256:" + 64 hex chars (got %q)`, to)
				}
			case "image":
				if !current && to == "" {
					return fmt.Errorf("image pins require either --to <sha256:digest> or --current to accept the reconciler-recorded drift\n" +
						"  --to <sha256:…>  set an explicit target digest\n" +
						"  --current        accept the digest observed by the reconciler (status.pin.details.observedDriftDigest)")
				}
				if to != "" && !refreezeIdentityRe.MatchString(to) {
					return fmt.Errorf(`--to must be "sha256:" + 64 hex chars (got %q)`, to)
				}
			case "skill", "cli":
				return fmt.Errorf("kind %q does not support pin-refreeze — its identity is declared in spec:\n"+
					"  skill: edit spec.source.resolvedSHA or the SkillSource\n"+
					"  cli:   edit spec.pinnedBinaryHash or versionRange", kindArg)
			default:
				return fmt.Errorf("unsupported kind %q; must be one of: mcp, image", kindArg)
			}

			b, err := g.Bundle()
			if err != nil {
				return err
			}
			return runPinUpdateOne(cmd.Context(), cmd.OutOrStdout(), b, kindArg, name, to, current)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "Target identity (sha256:… hash or image digest); required for image without --current, optional for mcp")
	cmd.Flags().BoolVar(&current, "current", false, "Accept the reconciler-recorded observed drift digest (status.pin.details.observedDriftDigest)")
	cmd.Flags().BoolVar(&all, "all", false, "Accept all drifted resources in the namespace")
	cmd.Flags().StringVar(&kindFilter, "kind", "", "With --all: restrict to one kind (mcp or image)")
	return cmd
}

// probeLiveHash probes the MCP server at the given URL and returns the
// canonical manifest hash of its tools/list response. It is used by both
// the single-resource and --all paths as a fallback when no recorded drift
// digest is available.
func probeLiveHash(ctx context.Context, serverURL, resourceName string, fallbackMsg string) (string, error) {
	pc := &probe.Client{URL: serverURL}
	liveTools, probeErr := pc.ListTools(ctx, "", "")
	if probeErr != nil {
		return "", fmt.Errorf("probe MCPServer %q failed%s: %w\n"+
			"The server may require authentication; pass --to with the hash from\n"+
			"a session's observedPins (see: oap session show <session>)",
			resourceName, fallbackMsg, probeErr)
	}
	liveHash, hashErr := mcppin.CanonicalManifestHash(liveTools)
	if hashErr != nil {
		return "", fmt.Errorf("compute live hash: %w", hashErr)
	}
	return liveHash, nil
}

// runPinUpdateOne implements `oap pin update <kind> <name> [--to <hash>|--current]`.
// It is extracted from RunE so tests can drive it directly against a fake client.
func runPinUpdateOne(ctx context.Context, out io.Writer, b *kube.Bundle, kind, name, to string, useCurrent bool) error {
	switch kind {
	case "mcp":
		identity := to
		if useCurrent {
			// Read the observed drift digest from the resource's status.
			var srv spiceboxv1alpha1.MCPServer
			if err := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, &srv); err != nil {
				return fmt.Errorf("get MCPServer %q: %w", name, err)
			}
			if srv.Status.Pin != nil && srv.Status.Pin.Details[detailKeyObservedDrift] != "" {
				identity = srv.Status.Pin.Details[detailKeyObservedDrift]
				if !refreezeIdentityRe.MatchString(identity) {
					return fmt.Errorf("recorded observedDriftDigest %q is not a valid sha256 digest; reconciler may need to re-run", identity)
				}
			} else {
				// Fall back to live probe when the key is absent (pre-Part-1 resources).
				var err error
				identity, err = probeLiveHash(ctx, srv.Spec.Server.URL, name, " (no recorded drift digest available)")
				if err != nil {
					return err
				}
			}
		} else if identity == "" {
			// Probe live unauthenticated to derive the hash.
			var srv spiceboxv1alpha1.MCPServer
			if err := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, &srv); err != nil {
				return fmt.Errorf("get MCPServer %q: %w", name, err)
			}
			var err error
			identity, err = probeLiveHash(ctx, srv.Spec.Server.URL, name, "")
			if err != nil {
				return err
			}
		}

		if err := setMCPServerRefreeze(ctx, b.Controller, b.Namespace, name, identity); err != nil {
			return err
		}
		if useCurrent {
			fmt.Fprintf(out, "accepting observed drift digest %s\n", identity)
		}
		fmt.Fprintf(out, "Set pin-refreeze annotation on MCPServer %q to %s\n", name, identity)

	case "image":
		var identity string
		if useCurrent {
			var sidecar spiceboxv1alpha1.SidecarToolbox
			if err := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, &sidecar); err != nil {
				return fmt.Errorf("get SidecarToolbox %q: %w", name, err)
			}
			if sidecar.Status.Pin == nil || sidecar.Status.Pin.Details[detailKeyObservedDrift] == "" {
				return fmt.Errorf("no observed drift recorded for SidecarToolbox %q; nothing to accept (is the resource actually drifted?)", name)
			}
			identity = sidecar.Status.Pin.Details[detailKeyObservedDrift]
			if !refreezeIdentityRe.MatchString(identity) {
				return fmt.Errorf("recorded observedDriftDigest %q is not a valid sha256 digest; reconciler may need to re-run", identity)
			}
			fmt.Fprintf(out, "accepting observed drift digest %s\n", identity)
		} else {
			identity = to
		}
		if err := setSidecarToolboxRefreeze(ctx, b.Controller, b.Namespace, name, identity); err != nil {
			return err
		}
		fmt.Fprintf(out, "Set pin-refreeze annotation on SidecarToolbox %q to %s\n", name, identity)
	}

	fmt.Fprintln(out, "The reconciler applies the refreeze within its revalidate interval (default 5m).")
	fmt.Fprintln(out, "Note: set imperatively; do not manage this annotation in GitOps manifests (the controller clears it on accept).")
	return nil
}

// runPinUpdateAll implements `oap pin update --all --current [--kind k]`.
// It lists MCPServers and SidecarToolboxes whose PinDrift condition is
// False/PinDrifted, reads the observed drift digest from each, and sets
// the refreeze annotation.
func runPinUpdateAll(ctx context.Context, cmd *cobra.Command, th *tui.Theme, b *kube.Bundle, kindFilter string) error {
	out := cmd.OutOrStdout()

	type row struct {
		kind       string
		name       string
		oldDigest  string
		newDigest  string
		skipReason string
	}
	var rows []row
	total := 0

	processMCP := kindFilter == "" || kindFilter == "mcp"
	processImage := kindFilter == "" || kindFilter == "image"

	if processMCP {
		var mcpList spiceboxv1alpha1.MCPServerList
		if err := b.Controller.List(ctx, &mcpList, client.InNamespace(b.Namespace)); err != nil {
			return fmt.Errorf("list MCPServers: %w", err)
		}
		for i := range mcpList.Items {
			srv := &mcpList.Items[i]
			if !isPinDrifted(srv.Status.Conditions) {
				continue
			}
			total++
			oldDigest := ""
			if srv.Status.Pin != nil {
				oldDigest = srv.Status.Pin.Digest
			}
			newDigest := ""
			if srv.Status.Pin != nil {
				newDigest = srv.Status.Pin.Details[detailKeyObservedDrift]
			}
			if newDigest == "" {
				// Fall back to live probe when the key is absent (pre-Part-1 resources).
				liveHash, probeErr := probeLiveHash(ctx, srv.Spec.Server.URL, srv.Name, "")
				if probeErr != nil {
					rows = append(rows, row{
						kind:       "MCPServer",
						name:       srv.Name,
						skipReason: fmt.Sprintf("probe/hash failed: %v", probeErr),
					})
					continue
				}
				newDigest = liveHash
			}
			if err := setMCPServerRefreeze(ctx, b.Controller, b.Namespace, srv.Name, newDigest); err != nil {
				rows = append(rows, row{
					kind:       "MCPServer",
					name:       srv.Name,
					skipReason: fmt.Sprintf("set refreeze failed: %v", err),
				})
				continue
			}
			rows = append(rows, row{
				kind:      "MCPServer",
				name:      srv.Name,
				oldDigest: oldDigest,
				newDigest: newDigest,
			})
		}
	}

	if processImage {
		var sidecarList spiceboxv1alpha1.SidecarToolboxList
		if err := b.Controller.List(ctx, &sidecarList, client.InNamespace(b.Namespace)); err != nil {
			return fmt.Errorf("list SidecarToolboxes: %w", err)
		}
		for i := range sidecarList.Items {
			sidecar := &sidecarList.Items[i]
			if !isPinDrifted(sidecar.Status.Conditions) {
				continue
			}
			total++
			oldDigest := ""
			if sidecar.Status.Pin != nil {
				oldDigest = sidecar.Status.Pin.Digest
			}
			newDigest := ""
			if sidecar.Status.Pin != nil {
				newDigest = sidecar.Status.Pin.Details[detailKeyObservedDrift]
			}
			if newDigest == "" {
				rows = append(rows, row{
					kind:       "SidecarToolbox",
					name:       sidecar.Name,
					skipReason: "no observed drift recorded; re-run after the reconciler has detected drift (status.pin.details.observedDriftDigest missing)",
				})
				continue
			}
			if !refreezeIdentityRe.MatchString(newDigest) {
				rows = append(rows, row{
					kind:       "SidecarToolbox",
					name:       sidecar.Name,
					skipReason: fmt.Sprintf("recorded observedDriftDigest %q is not a valid sha256 digest", newDigest),
				})
				continue
			}
			if err := setSidecarToolboxRefreeze(ctx, b.Controller, b.Namespace, sidecar.Name, newDigest); err != nil {
				rows = append(rows, row{
					kind:       "SidecarToolbox",
					name:       sidecar.Name,
					skipReason: fmt.Sprintf("set refreeze failed: %v", err),
				})
				continue
			}
			rows = append(rows, row{
				kind:      "SidecarToolbox",
				name:      sidecar.Name,
				oldDigest: oldDigest,
				newDigest: newDigest,
			})
		}
	}

	if total == 0 {
		fmt.Fprintln(out, "nothing drifted; no changes.")
		return nil
	}

	// Print summary table.
	accepted := 0
	t := tui.NewTable(th, "KIND", "NAME", "OLD-DIGEST", "->", "NEW-DIGEST", "STATUS")
	for _, r := range rows {
		if r.skipReason != "" {
			t.Row(r.kind, r.name, truncateDigest(r.oldDigest), "", "", "SKIPPED: "+r.skipReason)
		} else {
			accepted++
			t.Row(r.kind, r.name, truncateDigest(r.oldDigest), "->", truncateDigest(r.newDigest), "accepted")
		}
	}
	if _, err := fmt.Fprint(out, t.Render()); err != nil {
		return err
	}
	fmt.Fprintf(out, "accepted %d of %d drifted\n", accepted, total)
	// Table is already printed; return a non-zero exit so callers and CI can
	// detect a partial accept without having to parse the output.
	if accepted < total {
		return fmt.Errorf("accepted %d of %d drifted dependencies; %d skipped/failed (see rows above)", accepted, total, total-accepted)
	}
	return nil
}

// isPinDrifted returns true when the PinDrift condition is False/PinDrifted.
func isPinDrifted(conds []metav1.Condition) bool {
	c := meta.FindStatusCondition(conds, spiceboxv1alpha1.PinDriftCondition)
	if c == nil {
		return false
	}
	return c.Status == metav1.ConditionFalse && c.Reason == spiceboxv1alpha1.ReasonPinDrifted
}

// setMCPServerRefreeze sets the pin-refreeze annotation on the named MCPServer.
func setMCPServerRefreeze(ctx context.Context, c client.Client, namespace, name, identity string) error {
	var srv spiceboxv1alpha1.MCPServer
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &srv); err != nil {
		return fmt.Errorf("get MCPServer %q: %w", name, err)
	}
	if srv.Annotations == nil {
		srv.Annotations = make(map[string]string)
	}
	srv.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze] = identity
	if err := c.Update(ctx, &srv); err != nil {
		return fmt.Errorf("update MCPServer %q: %w", name, err)
	}
	return nil
}

// setSidecarToolboxRefreeze sets the pin-refreeze annotation on the named SidecarToolbox.
func setSidecarToolboxRefreeze(ctx context.Context, c client.Client, namespace, name, identity string) error {
	var sidecar spiceboxv1alpha1.SidecarToolbox
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sidecar); err != nil {
		return fmt.Errorf("get SidecarToolbox %q: %w", name, err)
	}
	if sidecar.Annotations == nil {
		sidecar.Annotations = make(map[string]string)
	}
	sidecar.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze] = identity
	if err := c.Update(ctx, &sidecar); err != nil {
		return fmt.Errorf("update SidecarToolbox %q: %w", name, err)
	}
	return nil
}
