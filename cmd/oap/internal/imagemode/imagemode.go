// Package imagemode decides where an oap build sends its images: local :dev
// tags loaded straight onto the node, or a registry push, and for which
// platform. Shared by `oap install`, `oap init`, `oap build`, `oap image` and
// `oap agent install`, which must all reach the same answer for one cluster.
package imagemode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// Resolve decides whether oap uses local :dev images or pushes to a
// registry, and which registry. Order: explicit --image-registry wins; then a
// profile that uses local dev images, or a kube-context whose node image store
// is reachable from the laptop; otherwise (remote) needsRemote=true so the
// caller can derive a suggestion and prompt, rather than hard-erroring.
//
// imageload.For stays: it classifies HOW a built image reaches the node
// (`kind load` vs `k3d import` vs SSH-to-VM vs registry), which is orthogonal
// to the cluster kind and total over kube-contexts.
func Resolve(reg string, p cloud.InstallProfile, kctx string) (registry string, useLocal bool, needsRemote bool, err error) {
	if reg != "" {
		return reg, false, false, nil
	}
	if p.UsesLocalDevImages() || imageload.For(kctx, "").Disposition != imageload.NeedsRegistry {
		return "", true, false, nil
	}
	// When context resolution failed (kctx == "") and the profile does not
	// require an external hostname, the cluster is local/undetectable — treat
	// it as local rather than emitting a spurious "remote needs a registry"
	// error.
	//
	// RequiresExternalHostname() happens to equal IsManaged() across all five
	// registered kinds today (pinned by
	// TestCharacterizationHostnameGuardOverLocalModeAndKind), but the two
	// answer different questions — hostname routing reachability vs. registry
	// availability — and nothing forces them to keep agreeing. A future kind
	// that requires an external hostname without being a managed cloud (or the
	// reverse) would silently change this branch's behavior. If that
	// divergence ever materializes, give this branch its own dedicated profile
	// method instead of continuing to lean on the coincidence.
	if kctx == "" && !p.RequiresExternalHostname() {
		return "", true, false, nil
	}
	return "", false, true, nil
}

// resolveRemoteRegistry turns a remote cluster with no --image-registry into a
// registry: interactive → prompt (suggestion prefilled); non-interactive →
// error (naming the suggestion if any). prompt is injected for testing; the
// production prompt reads stdin.
func resolveRemoteRegistry(suggestion string, interactive bool, prompt func(label, def string) (string, error)) (string, error) {
	if !interactive {
		if suggestion != "" {
			return "", fmt.Errorf("remote cluster needs an image registry: pass --image-registry (suggested: %s)", suggestion)
		}
		return "", fmt.Errorf("remote cluster needs an image registry: pass --image-registry <registry>")
	}
	reg, err := prompt("Image registry for this cluster", suggestion)
	if err != nil {
		return "", err
	}
	reg = strings.TrimSpace(reg)
	if reg == "" {
		return "", fmt.Errorf("no image registry provided")
	}
	return reg, nil
}

// Prompt reads an image registry from stdin, offering def as the
// default (Enter accepts it).
func Prompt(out io.Writer, in io.Reader) func(label, def string) (string, error) {
	return func(label, def string) (string, error) {
		if def != "" {
			cliout.Prompt(out, "%s [%s]: ", label, def)
		} else {
			cliout.Prompt(out, "%s: ", label)
		}
		sc := bufio.NewScanner(in)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", err
			}
			return def, nil // EOF → accept default
		}
		ans := strings.TrimSpace(sc.Text())
		if ans == "" {
			return def, nil
		}
		return ans, nil
	}
}

// FirstNodeInfo lists one cluster node and returns its architecture +
// providerID. found is false when the cluster has zero nodes (err stays nil —
// the caller decides whether an empty cluster is fatal); err is non-nil only on
// a List failure.
func FirstNodeInfo(ctx context.Context, kc kubernetes.Interface) (arch, providerID string, found bool, err error) {
	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return "", "", false, err
	}
	if len(nodes.Items) == 0 {
		return "", "", false, nil
	}
	n := nodes.Items[0]
	return n.Status.NodeInfo.Architecture, n.Spec.ProviderID, true, nil
}

// DetectNodeArch returns a cluster node's architecture ("amd64"/"arm64"), or ""
// if undetectable (caller defaults to amd64).
func DetectNodeArch(ctx context.Context, kc kubernetes.Interface) string {
	arch, _, _, _ := FirstNodeInfo(ctx, kc)
	return arch
}

// ResolveRemoteFromCluster resolves the image registry for a remote
// cluster started without --image-registry, reading stdin for the prompt.
// It is the production adapter over ResolveForCluster; see that
// function for the resolution order and why each step exists.
func ResolveRemoteFromCluster(ctx context.Context, kc kubernetes.Interface, c registryDeriver, kctx string, out io.Writer, in *os.File) (string, error) {
	return ResolveForCluster(ctx, kc, c, kctx, "", out, Prompt(out, in), apcmd.StdinIsInteractive(in))
}

// deriveRegistry returns a best-effort image-registry suggestion derived from
// the cluster's cloud + a node providerID, or "" when none can be derived. The
// per-cloud derivation lives on the cloud strategy's RegistryFromProviderID
// method (only GKE is fully derivable today; EKS/AKS return "" and the caller
// prompts).
func deriveRegistry(c interface{ RegistryFromProviderID(string) string }, providerID string) string {
	return c.RegistryFromProviderID(providerID)
}

// registryDeriver is the slice of cloud.Strategy this file needs: turning a
// node providerID into a registry suggestion.
type registryDeriver interface {
	RegistryFromProviderID(string) string
}

// registryFromOperatorImage returns the registry root of the installed operator
// Deployment's image — the cluster's own answer to "which registry do I pull
// from", and therefore the best default for pushing anything else it must pull.
//
// Returns ("", nil) when the operator is not installed (a fresh cluster, or
// `oap agent install` run before `oap install`) or when it runs a bare local tag
// (a :dev image on a local cluster): both are ordinary "no answer here" cases
// the caller handles by falling through, not failures.
//
// It reads ONE deterministic object rather than scanning first-party workloads,
// so the answer cannot vary with list ordering.
func registryFromOperatorImage(ctx context.Context, kc kubernetes.Interface) (string, error) {
	d, err := kc.AppsV1().Deployments(apcmd.SystemNamespace).Get(ctx, apcmd.OperatorDeployment, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s/%s to infer the image registry: %w", apcmd.SystemNamespace, apcmd.OperatorDeployment, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if root, ok := RegistryRootOf(c.Image); ok {
			return root, nil
		}
	}
	return "", nil
}

// ResolveForCluster resolves the image registry a cluster pulls from,
// in order:
//
//  1. an explicit --image-registry flag
//  2. the installed operator Deployment's own image registry (no prompt)
//  3. a cloud-derived suggestion from a node's providerID
//  4. an interactive prompt prefilled with (3), or a fail-closed error
//
// Step 2 is what lets a second install auto-fill: it reflects where the control
// plane's images ACTUALLY came from, not where they conventionally would, and a
// registry the cluster already pulls its control plane from is one its nodes
// demonstrably have read access to.
//
// prompt and interactive are injected so the decision table is testable without
// a TTY; production callers pass Prompt(out, in) and
// apcmd.StdinIsInteractive(in).
func ResolveForCluster(
	ctx context.Context,
	kc kubernetes.Interface,
	c registryDeriver,
	kctx, flagRegistry string,
	out io.Writer,
	prompt func(label, def string) (string, error),
	interactive bool,
) (string, error) {
	if flagRegistry != "" {
		return flagRegistry, nil
	}

	reg, err := registryFromOperatorImage(ctx, kc)
	if err != nil {
		// Not fatal: steps 3-4 can still resolve a registry. But an operator we
		// could not read is worth saying out loud, because it silently changes
		// which registry we land on.
		cliout.Warn(out, "could not infer the image registry from the installed operator: %v — falling back to cloud detection", err)
	}
	if reg != "" {
		cliout.Step(out, "image registry (from the installed operator image): %s", reg)
		return reg, nil
	}

	_, providerID, found, err := FirstNodeInfo(ctx, kc)
	if err != nil {
		return "", fmt.Errorf("inspect nodes of cluster %q: %w", kctx, err)
	}
	if !found {
		return "", fmt.Errorf("cluster %q has no nodes, so its cloud type and image registry can't be detected — ensure it has running nodes (an Autopilot cluster may have scaled to zero, or its node auto-provisioner may be wedged), or pass --image-registry explicitly", kctx)
	}
	return resolveRemoteRegistry(deriveRegistry(c, providerID), interactive, prompt)
}

// ResolvePushPlatform picks the --platform value for a registry push. An
// explicit flag always wins; otherwise the target is the CLUSTER's nodes, so it
// follows nodeArch, falling back to linux/amd64 when that is undetectable.
//
// It deliberately never falls back to the docker host's architecture. That
// default is correct only for a local load, where the node IS the host. For a
// push it is silently wrong: building on an arm64 laptop for amd64 nodes yields
// an image that pushes and pulls cleanly, then fails with "exec format error"
// the first time a container starts — far from the command that caused it.
func ResolvePushPlatform(flagPlatform, nodeArch string) string {
	if flagPlatform != "" {
		return flagPlatform
	}
	if nodeArch != "" {
		return "linux/" + nodeArch
	}
	return "linux/amd64"
}
