package health

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
)

func init() {
	// Verifies the memory-token Secrets exist. This is the GitOps-specific check:
	// `oap install` creates these imperatively, but a bundle synced by Argo/Flux
	// relies on the pre-start bootstrap Job (the operator image's
	// `bootstrap-secrets` entrypoint) to mint them — so their absence is the
	// signal that the Job never completed (wrong image, backoff exhausted, the
	// spicebox-bootstrap Role missing Secret RBAC, or a NetworkPolicy blocking its
	// apiserver egress).
	Register(memoryTokenSecrets{})
}

// memoryTokenSecrets checks that each component's memory-token Secret exists and
// carries a non-empty token. It checks the Secret each Deployment ACTUALLY
// references, so a token linked with `oap render --*-memory-token-secret` is
// verified rather than the default name the bootstrap Job mints regardless;
// a component whose Deployment is absent falls back to its default name. The
// other app-internal secrets the Job mints (admind, passthrough, NATS,
// spicedb/postgres) are verified indirectly by the workload health checks that
// mount them.
type memoryTokenSecrets struct{}

// memoryTokenConsumer locates one component's memory-token reference: a volume
// (channelsd, webd mount it as a file) or an env var (authzd).
type memoryTokenConsumer struct {
	deployment    string
	defaultSecret string
	volume        string
	env           string
}

var memoryTokenConsumers = []memoryTokenConsumer{
	{deployment: "spicebox-channelsd", defaultSecret: bootstrap.ChannelsdMemoryTokenSecret, volume: "memory-token"},
	{deployment: "spicebox-webd", defaultSecret: bootstrap.WebdMemoryTokenSecret, volume: "memory-token"},
	{deployment: "agentprimitives-authzd", defaultSecret: bootstrap.AuthzdMemoryTokenSecret, env: "MEMORY_TOKEN"},
}

// secretName returns the memory-token Secret the live Deployment references,
// or the default when the Deployment (or the reference) is absent.
func (c memoryTokenConsumer) secretName(ctx context.Context, b *kube.Bundle) (string, error) {
	dep, err := b.Typed.AppsV1().Deployments(namespace).Get(ctx, c.deployment, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return c.defaultSecret, nil
	}
	if err != nil {
		return "", fmt.Errorf("get deployment %s: %w", c.deployment, err)
	}
	if name := c.referenced(&dep.Spec.Template.Spec); name != "" {
		return name, nil
	}
	return c.defaultSecret, nil
}

func (c memoryTokenConsumer) referenced(spec *corev1.PodSpec) string {
	for _, v := range spec.Volumes {
		if c.volume != "" && v.Name == c.volume && v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	for _, ctr := range spec.Containers {
		for _, e := range ctr.Env {
			if c.env != "" && e.Name == c.env && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				return e.ValueFrom.SecretKeyRef.Name
			}
		}
	}
	return ""
}

func (memoryTokenSecrets) Name() string { return "memory-token-secrets" }

func (memoryTokenSecrets) Check(ctx context.Context, b *kube.Bundle) Result {
	var missing []string
	present := 0
	for _, consumer := range memoryTokenConsumers {
		name, err := consumer.secretName(ctx, b)
		if err != nil {
			return Result{Status: Failed, Detail: err.Error()}
		}
		sec, err := b.Typed.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, name)
		case err != nil:
			return Result{Status: Failed, Detail: fmt.Sprintf("get %s: %v", name, err)}
		case len(sec.Data[bootstrap.TokenSecretKey]) == 0:
			missing = append(missing, name+" (empty token)")
		default:
			present++
		}
	}
	total := len(memoryTokenConsumers)
	switch {
	case len(missing) == 0:
		return Result{Status: OK, Detail: fmt.Sprintf("%d/%d memory tokens present", present, total)}
	case present == 0:
		// None present: the bootstrap Job has not minted them. NotFound lets a
		// trimmed profile skip, matching the workload checks.
		return Result{Status: Failed, Detail: "no memory tokens present (the bootstrap Job has not minted them)", NotFound: true}
	default:
		return Result{Status: Failed, Detail: "missing memory tokens: " + strings.Join(missing, ", ")}
	}
}
