package installcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/health"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

// operatorNS is apcmd.SystemNamespace under the name this command tree has
// always used for it.
const operatorNS = apcmd.SystemNamespace

var requiredCRDs = []string{
	"agentclasses.agentprimitives.authzed.com",
	"agentsessions.agentprimitives.authzed.com",
	"agentidentities.agentprimitives.authzed.com",
	"spiceboxclasses.agentprimitives.authzed.com",
	"spiceboxsessions.agentprimitives.authzed.com",
	"spiceboxtoolspecs.agentprimitives.authzed.com",
	"spiceboxtoolkits.agentprimitives.authzed.com",
	"toolcalls.agentprimitives.authzed.com",
}

func NewCheckCmd(g *apcmd.Globals) *cobra.Command {
	watch := false
	repair := false
	noImageCheck := false
	timeout := 60 * time.Second

	cmd := &cobra.Command{
		Use:   "check",
		Short: "verify namespace, CRDs, services, and every registered component's health",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			run := func() error { return runCheck(ctx, out, b, repair) }
			var coreErr error
			if !watch {
				coreErr = run()
			} else {
				coreErr = wait.Until(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
					if err := run(); err == nil {
						return true, nil
					}
					return false, nil
				})
			}
			// Image-digest drift runs ONCE here — deliberately outside runCheck
			// (shared with `oap init`'s 2s poll loop) and outside the --watch retry
			// loop, since resolving each image's digest hits the registry via
			// docker/buildx. Warn-only: it never changes the check's exit code.
			if !noImageCheck {
				if derr := checkImageDrift(ctx, out, b, repair); derr != nil {
					fmt.Fprintf(out, "⚠ image-digest drift check skipped: %v\n", derr)
				}
			}
			return coreErr
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "Re-check every 2s until pass or timeout")
	cmd.Flags().BoolVar(&repair, "repair", false, "Attempt to repair failed components (rollout-restart; re-pin drifted images) then re-check")
	cmd.Flags().BoolVar(&noImageCheck, "no-image-check", false, "Skip the image-digest drift check (which resolves registry digests via docker/buildx)")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "Overall timeout when --watch is set")
	return cmd
}

func runCheck(ctx context.Context, out io.Writer, b *kube.Bundle, repair bool) error {
	var errs []error
	pass := func(msg string) { fmt.Fprintln(out, "✓", msg) }
	warn := func(msg string) { fmt.Fprintln(out, "⚠", msg) }
	fail := func(msg string) { fmt.Fprintln(out, "✗", msg); errs = append(errs, errors.New(msg)) }

	// 1. namespace
	if _, err := b.Typed.CoreV1().Namespaces().Get(ctx, operatorNS, metav1.GetOptions{}); err == nil {
		pass(fmt.Sprintf("namespace %s exists", operatorNS))
	} else {
		fail(fmt.Sprintf("namespace %s missing: %v", operatorNS, err))
	}

	// 2. CRDs
	for _, name := range requiredCRDs {
		var crd apiextv1.CustomResourceDefinition
		if err := b.Controller.Get(ctx, client.ObjectKey{Name: name}, &crd); err == nil {
			pass(fmt.Sprintf("CRD %s installed", name))
		} else {
			fail(fmt.Sprintf("CRD %s missing: %v", name, err))
		}
	}

	// 3. components (registry-driven): each defines its own health check, and
	// optionally a repair that --repair invokes on failure. This is what now
	// catches a CrashLoopBackOff authzd/channelsd/etc. — previously unchecked.
	for _, c := range health.All() {
		res := c.Check(ctx, b)
		switch res.Status {
		case health.OK:
			pass(fmt.Sprintf("%s: %s", c.Name(), res.Detail))
		case health.Optional:
			warn(fmt.Sprintf("%s: %s (optional, non-fatal)", c.Name(), res.Detail))
		case health.Failed:
			fail(fmt.Sprintf("%s: %s", c.Name(), res.Detail))
			if repair {
				rp, ok := c.(health.Repairer)
				if !ok {
					warn(fmt.Sprintf("%s: no repair available", c.Name()))
					break
				}
				if err := rp.Repair(ctx, b); err != nil {
					warn(fmt.Sprintf("%s: repair failed: %v", c.Name(), err))
				} else {
					warn(fmt.Sprintf("%s: repair attempted — re-run oap check to verify", c.Name()))
				}
			}
		}
	}

	// 4. services
	for _, svc := range []string{"spicebox-operator", "spicebox-gateway"} {
		if _, err := b.Typed.CoreV1().Services(operatorNS).Get(ctx, svc, metav1.GetOptions{}); err == nil {
			pass(fmt.Sprintf("service %s exists", svc))
		} else {
			fail(fmt.Sprintf("service %s missing: %v", svc, err))
		}
	}

	// 5. healthz: deferred. Healthz probe deferred; the deployment-ready check above is the proxy.

	if len(errs) > 0 {
		return fmt.Errorf("oap check: %d failure(s)", len(errs))
	}
	pass("all checks passed")
	return nil
}
