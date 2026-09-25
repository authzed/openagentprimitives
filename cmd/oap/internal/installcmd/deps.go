package installcmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// readSecretLine prompts on out and reads one credential line from in with
// terminal echo suppressed, so the value never appears on screen or in
// scrollback. Returns the trimmed value and whether echo was actually
// suppressed.
//
// fallback is the scanner the caller is ALREADY reading in with. It is used
// when there is no terminal to suppress echo on, so the caller's buffered-ahead
// input is not stranded in a reader this function threw away — the failure mode
// a second, private bufio.Scanner over the same io.Reader would introduce.
//
// This is the repo's existing non-huh secret-line shape (see readSecretLine in
// pkg/platform/identity/setup/llmagent/tools/prompt_user.go). huh's EchoModePassword is
// deliberately NOT used: that mode needs a reader with a terminal file
// descriptor and huh's accessible renderer discards the error when there is
// none, so off-TTY the field asks nothing, accepts nothing and reports nothing
// — the reason three other call sites in this repo already refuse it. A prompt
// that a scripted install can still answer has to degrade to a plain read
// instead, which is what the fallback branch does.
//
// echoSuppressed=false is not an error: it is the honest report that the value
// was typed in the clear. The caller decides whether to warn (resolveExternalSpiceDB
// does, because its value is the SpiceDB root credential).
func readSecretLine(in io.Reader, fallback *bufio.Scanner, out io.Writer, format string, a ...any) (value string, echoSuppressed bool) {
	cliout.Prompt(out, format, a...)
	if f := terminalFile(in); f != nil {
		raw, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			// Do NOT silently retry with an echoing read: the caller asked for
			// a secret, and downgrading without saying so is how the value ends
			// up on screen anyway. Report it and return nothing.
			cliout.Warn(out, "could not read the value with terminal echo off: %v", err)
			return "", false
		}
		fmt.Fprintln(out) // the user's Enter was not echoed; close the line ourselves
		return strings.TrimSpace(string(raw)), true
	}
	if !fallback.Scan() {
		if err := fallback.Err(); err != nil {
			cliout.Warn(out, "could not read the value: %v", err)
		}
		return "", false
	}
	return strings.TrimSpace(fallback.Text()), false
}

// terminalFile returns the os.File behind r when r IS this process's own
// terminal, and nil otherwise — a piped or redirected stdin, a file, or any
// non-*os.File reader (every test in this package).
//
// The IsTerminal check is the load-bearing half: stdin redirected from a file
// is still an *os.File, and calling term.ReadPassword on it would fail with
// ENOTTY rather than read the line a scripted install expects.
func terminalFile(r io.Reader) *os.File {
	f, ok := r.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return f
}

// component describes a cluster-wide dependency oap can install on the operator's
// behalf (cert-manager, Envoy Gateway).
type component struct {
	name      string                              // human name for prompts/logs
	why       string                              // one-line reason it's needed
	creates   string                              // what installing it adds (namespaces/CRDs/etc.)
	manualCmd string                              // exact command to run instead
	present   func(context.Context) (bool, error) // already installed?
	manifests func() ([][]byte, error)            // embedded bundle
	ready     func(context.Context) error         // post-apply readiness wait
}

// ensureComponent installs comp if not already present, after explaining what it
// will do and (unless assumeYes) getting consent. Returns (present, err): present
// is true if the component is usable afterwards. On decline it prints the manual
// command and returns (false, nil) so the caller can skip dependent steps.
func ensureComponent(ctx context.Context, out io.Writer, in io.Reader, bundle *kube.Bundle, comp component, assumeYes, isTTY bool) (bool, error) {
	ok, err := comp.present(ctx)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", comp.name, err)
	}
	if ok {
		fmt.Fprintf(out, "  %s already present\n", comp.name)
		return true, nil
	}
	fmt.Fprintf(out, "\n%s is not installed.\n  why: %s\n  installing it creates: %s\n  manual alternative: %s\n",
		comp.name, comp.why, comp.creates, comp.manualCmd)
	if !apcmd.Confirm(in, out, fmt.Sprintf("Install %s now?", comp.name), assumeYes, isTTY) {
		fmt.Fprintf(out, "  skipped %s; run the command above, then re-run oap install.\n", comp.name)
		return false, nil
	}
	groups, err := comp.manifests()
	if err != nil {
		return false, fmt.Errorf("load %s manifests: %w", comp.name, err)
	}
	cliout.Step(out, "apply %s", comp.name)
	for _, g := range groups {
		docs, derr := manifests.Split(g)
		if derr != nil {
			return false, fmt.Errorf("split %s manifest: %w", comp.name, derr)
		}
		for _, d := range docs {
			// Make the component leader-election-free so it works on GKE
			// Autopilot, where the bundled cert-manager/Envoy default of a
			// kube-system leader-election lease is forbidden (cainjector then
			// never leads → the webhook CA is never injected).
			if err := disableLeaderElection(d); err != nil {
				return false, fmt.Errorf("%s: %w", comp.name, err)
			}
			if err := kube.Apply(ctx, bundle.Dynamic, d, "ap-install"); err != nil {
				return false, fmt.Errorf("apply %s %s/%s: %w", comp.name, d.GetKind(), d.GetName(), err)
			}
		}
	}
	if comp.ready != nil {
		if err := comp.ready(ctx); err != nil {
			return false, fmt.Errorf("%s did not become ready: %w", comp.name, err)
		}
	}
	fmt.Fprintf(out, "  installed %s\n", comp.name)
	return true, nil
}

// hasCRD reports whether the cluster serves the given CRD (group/resource), used
// to detect cert-manager / Gateway API presence.
func hasCRD(ctx context.Context, b *kube.Bundle, gvr schema.GroupVersionResource) (bool, error) {
	_, err := b.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err) || cloud.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}

// envoyGatewayComponent describes Envoy Gateway for the ensure flow.
func envoyGatewayComponent(b *kube.Bundle) component {
	gatewayClassGVR := schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses",
	}
	return component{
		name:      "Envoy Gateway",
		why:       "implements the Gateway API (no usable GatewayClass detected on this cluster)",
		creates:   "namespace envoy-gateway-system, Gateway API CRDs, and the Envoy Gateway controller Deployment",
		manualCmd: "kubectl apply -f https://github.com/envoyproxy/gateway/releases/download/v1.2.4/install.yaml",
		present: func(ctx context.Context) (bool, error) {
			return hasCRD(ctx, b, gatewayClassGVR)
		},
		manifests: manifests.EnvoyGateway,
		ready: func(ctx context.Context) error {
			return wait.ForDeployment(ctx, b.Typed, "envoy-gateway-system", "envoy-gateway", 1)
		},
	}
}

// envoyGatewayCloudComponent wraps envoyGatewayComponent as a cloud.Component for
// use with cloud.EnsureComponent in the rewired install flow.
func envoyGatewayCloudComponent(b *kube.Bundle) cloud.Component {
	c := envoyGatewayComponent(b)
	return cloud.Component{
		Name:      c.name,
		Why:       c.why,
		Creates:   c.creates,
		ManualCmd: c.manualCmd,
		Present:   c.present,
		Manifests: c.manifests,
		Ready:     c.ready,
	}
}

// disableLeaderElection appends --leader-elect=false to any Deployment container
// that runs with a --leader-election-namespace flag. The bundled cert-manager
// and Envoy Gateway default leader-election to kube-system, which GKE Autopilot
// forbids writing to — so the cainjector/controller never acquire leadership and
// (for cert-manager) the validating webhook's CA is never injected, breaking
// ClusterIssuer creation with "x509: certificate signed by unknown authority".
// These are single-replica installs, so disabling leader election is safe on
// every cluster. No-op for non-Deployment docs and containers without the flag.
func disableLeaderElection(doc *unstructured.Unstructured) error {
	if doc.GetKind() != "Deployment" {
		return nil
	}
	containers, found, err := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("disable leader election: read %s containers: %w", doc.GetName(), err)
	}
	if !found {
		return nil
	}
	changed := false
	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		args, _ := cm["args"].([]any)
		usesLeaderElection, alreadyDisabled := false, false
		for _, a := range args {
			s, _ := a.(string)
			if strings.HasPrefix(s, "--leader-election-namespace") {
				usesLeaderElection = true
			}
			if s == "--leader-elect=false" {
				alreadyDisabled = true
			}
		}
		if usesLeaderElection && !alreadyDisabled {
			cm["args"] = append(args, "--leader-elect=false")
			containers[i] = cm
			changed = true
		}
	}
	if changed {
		if err := unstructured.SetNestedSlice(doc.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("disable leader election: write %s args: %w", doc.GetName(), err)
		}
	}
	return nil
}

// isInteractive reports whether stdin is a terminal (so prompting is sensible).
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}
