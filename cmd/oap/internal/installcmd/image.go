package installcmd

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
)

// NewImageCmd is `oap image`: helpers for getting locally-built images into the
// cluster the current kube-context points at, without a registry.
func NewImageCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Work with local Docker images and the current cluster",
	}
	cmd.AddCommand(newImageLoadCmd(g))
	return cmd
}

// newImageLoadCmd is `oap image load <image[:tag]>`: load a locally-built Docker
// image into the current cluster's node(s) with no registry round-trip. It is
// the standalone form of what `oap agent install` does for a bundle's custom
// images (see loadImageIntoCluster / installEnv.Deliver) — useful for iterating
// on an image outside of an install.
func newImageLoadCmd(g *apcmd.Globals) *cobra.Command {
	var vmSSHKey string
	cmd := &cobra.Command{
		Use:   "load <image[:tag]>",
		Short: "Load a local Docker image into the current cluster (no registry)",
		Long: "Load a locally-built Docker image into the node(s) of the cluster the\n" +
			"current kube-context points at, without pushing to a registry:\n\n" +
			"  - oap-desktop:           docker save -> SSH -> k3s ctr images import (into the VM)\n" +
			"  - kind / k3d / minikube: the runtime's own image-import command\n" +
			"  - docker-desktop:        no-op (the node shares this machine's Docker daemon)\n" +
			"  - anything else:         an error — those nodes can only pull from a registry,\n" +
			"                           so there is nothing this command can do\n\n" +
			"The image must already exist in the local Docker daemon — build it first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return loadImageIntoCluster(cmd.Context(), cmd.OutOrStdout(), g, args[0], vmSSHKey)
		},
	}
	cmd.Flags().StringVar(&vmSSHKey, "vm-ssh-key", "",
		"Path to the oap-desktop VM SSH private key (default: auto-discover; overrides OAP_DESKTOP_SSH_KEY)")
	return cmd
}

// loadImageIntoCluster makes a locally-built image tag available to the cluster
// the current kube-context points at, with no registry: the oap-desktop VM
// (docker save -> SSH -> `k3s ctr images import`), a kind/k3d/minikube node
// (imageload.For's LocalLoad runtime command), a no-op + note for
// docker-desktop, or — for a context whose nodes can only pull from a registry
// — an error, since there is no registry-free way to satisfy the request. It
// mirrors installEnv.Deliver's local-load dispatch, resolving a fresh
// connection each call (the install path reuses a pre-resolved one); both
// funnel through the same desktop.LoadImage / imageload.For primitives.
// vmSSHKey overrides the oap-desktop VM key (else VMKeyPath /
// OAP_DESKTOP_SSH_KEY auto-discovery).
func loadImageIntoCluster(ctx context.Context, out io.Writer, g *apcmd.Globals, tag, vmSSHKey string) error {
	kctx, err := g.CurrentContext()
	if err != nil {
		return fmt.Errorf("resolve kube-context: %w", err)
	}
	// One switch over the total classification: "which context is the VM" is a
	// fact imageload owns, and re-deriving it here from the context string
	// (as this function used to) is the second representation that drifted last
	// time. Argv is non-nil only for LocalLoad, so exec'ing it belongs in that
	// case and nowhere else.
	plan := imageload.For(kctx, tag)
	switch plan.Disposition {
	case imageload.NotNeeded:
		fmt.Fprintf(out, "  (context %q shares this machine's Docker daemon — no load needed)\n", kctx)
		return nil
	case imageload.NeedsRegistry:
		return fmt.Errorf("context %q has no local image-load path — its nodes can only get %s from a registry. Build and push it with `oap build --image-registry <registry>` (or `oap agent install --image-registry <registry>` for a bundle image)", kctx, tag)
	case imageload.VMLoad:
		raw, err := g.KubeconfigRaw()
		if err != nil {
			return err
		}
		guestIP, err := desktop.GuestIPFromKubeconfig(raw, kctx)
		if err != nil {
			return err
		}
		key := vmSSHKey
		if key == "" {
			if key, err = desktop.VMKeyPath(); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "==> loading %s into the oap-desktop VM\n", tag)
		return desktop.LoadImage(ctx, desktop.NewSSHRunner(), desktop.NewDockerSaver(), key, guestIP, tag)
	case imageload.LocalLoad:
		fmt.Fprintf(out, "==> %s\n", strings.Join(plan.Argv, " "))
		c := exec.CommandContext(ctx, plan.Argv[0], plan.Argv[1:]...)
		c.Stdout, c.Stderr = out, out
		if err := c.Run(); err != nil {
			return fmt.Errorf("image-load %s: %w", tag, err)
		}
		return nil
	default:
		// Unreachable while imageload's classification stays total; adding a
		// disposition must surface as an error here, not as an index into the
		// nil Argv that every non-LocalLoad disposition carries.
		return fmt.Errorf("unhandled image-load disposition %s for context %q: cannot load %s", plan.Disposition, kctx, tag)
	}
}
