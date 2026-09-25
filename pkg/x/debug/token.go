package debug

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TokenSecretName is the canonical Secret name holding the debug bearer token.
const TokenSecretName = "spicebox-operator-debug-token"

// EnsureToken returns the debug bearer token, generating + storing one if the Secret
// does not yet exist in the given namespace. Uses an uncached client (no informer
// cache needed before manager start).
func EnsureToken(ctx context.Context, c client.Client, namespace string) (string, error) {
	key := types.NamespacedName{Name: TokenSecretName, Namespace: namespace}

	var existing corev1.Secret
	err := c.Get(ctx, key, &existing)
	switch {
	case err == nil:
		token, ok := existing.Data["token"]
		if !ok || len(token) == 0 {
			return "", fmt.Errorf("secret %s exists but lacks 'token' key", key)
		}
		return string(token), nil
	case errors.IsNotFound(err):
		// Fall through to create.
	default:
		return "", fmt.Errorf("get %s: %w", key, err)
	}

	token, err := generateToken()
	if err != nil {
		return "", err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      TokenSecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "spicebox-operator",
				"app.kubernetes.io/component": "debug",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": []byte(token)},
	}
	if err := c.Create(ctx, sec); err != nil {
		if errors.IsAlreadyExists(err) {
			// Racy create: another operator replica beat us. Re-read.
			if getErr := c.Get(ctx, key, &existing); getErr == nil {
				if v, ok := existing.Data["token"]; ok && len(v) > 0 {
					return string(v), nil
				}
			}
		}
		return "", fmt.Errorf("create %s: %w", key, err)
	}
	return token, nil
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
