// Package modeltoken resolves a model API-token value from a file, env var, or
// interactive prompt, and server-side-applies it into a central Secret that the
// operator can read (carrying the adoption label).
package modeltoken

import (
	"context"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

// Resolve returns the token value in precedence order: the file at `file`
// (trimmed), then the env var named `env` (when non-empty), then `prompt` when
// it is non-nil (interactive). Returns a fail-closed error naming the env var
// when no source yields a value — never silently empty.
func Resolve(file, env string, prompt func() (string, error)) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read model token file %q: %w", file, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("model token file %q is empty", file)
		}
		return v, nil
	}
	if env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v, nil
		}
	}
	if prompt != nil {
		v, err := prompt()
		if err != nil {
			return "", err
		}
		if v = strings.TrimSpace(v); v == "" {
			return "", fmt.Errorf("no model token entered")
		}
		return v, nil
	}
	return "", fmt.Errorf("no model token: set --default-model-token-file, export $%s, or run interactively", env)
}

// SecretRef identifies the central token Secret.
type SecretRef struct{ Namespace, Name, Key string }

// EnsureSecret server-side-applies an Opaque Secret holding `value` under
// ref.Key AND the operator adoption label, so adoptguard.SecretReader accepts
// it (an unlabelled Secret makes the operator panic). Idempotent: re-applying
// converges under the oap-settings-wizard field manager.
func EnsureSecret(ctx context.Context, dyn dynamic.Interface, ref SecretRef, value string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      ref.Name,
			"namespace": ref.Namespace,
			"labels":    map[string]any{adoptguard.AdoptedLabel: "true"},
		},
		"type":       "Opaque",
		"stringData": map[string]any{ref.Key: value},
	}}
	if err := kube.Apply(ctx, dyn, obj, "ap-settings-wizard"); err != nil {
		return fmt.Errorf("apply central model-token secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return nil
}
