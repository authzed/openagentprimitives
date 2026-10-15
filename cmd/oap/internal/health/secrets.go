package health

import (
	"context"
	"fmt"
	"strings"

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
	// signal that the Job never completed (wrong image, backoff exhausted, or the
	// operator ServiceAccount is missing Secret RBAC).
	Register(memoryTokenSecrets{})
}

// memoryTokenSecrets checks that each memory-token Secret exists and carries a
// non-empty token. It covers the three memory tokens (bootstrap.MemoryTokenSecretNames)
// the bootstrap Job mints; the other app-internal secrets the Job mints (admind,
// passthrough, NATS, spicedb/postgres) are verified indirectly by the workload
// health checks that mount them.
type memoryTokenSecrets struct{}

func (memoryTokenSecrets) Name() string { return "memory-token-secrets" }

func (memoryTokenSecrets) Check(ctx context.Context, b *kube.Bundle) Result {
	var missing []string
	present := 0
	for _, name := range bootstrap.MemoryTokenSecretNames {
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
	total := len(bootstrap.MemoryTokenSecretNames)
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
