// Package bootstrap holds the once-only, against-empty-state install steps that
// must run in exactly one place. Minting the memory-token Secrets lives here so
// both oap install's fast path and the operator's leader-only bootstrap Runnable
// invoke the SAME idempotent logic — the token is write-once and never rotated,
// so two Create-if-absent callers cannot drift.
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

const (
	// TokenSecretNamespace is the namespace every bootstrap token Secret lives in.
	TokenSecretNamespace = "agentprimitives-system"
	// TokenSecretKey is the single data key each bootstrap token Secret carries.
	TokenSecretKey = "token"
)

// MemoryTokenSecretNames is the canonical set of per-component memory-token
// Secrets the bundle requires. Kept in sync with install.go's
// operatorMemoryTokenSecrets and secret_bootstrap_test.go's installTimeSecrets.
var MemoryTokenSecretNames = []string{
	"spicebox-channelsd-memory-token",
	"spicebox-webd-memory-token",
	"agentprimitives-authzd-memory-token",
}

// EnsureTokenSecret creates a `token`-keyed random-bearer Secret if absent, and
// is a no-op (created=false) if it already exists — it never rotates. reader
// must be a live/uncached reader (the operator's Secret cache is label-filtered
// and would hide an as-yet-unlabeled Secret). AlreadyExists from Create — a
// leader-failover race, or a cache miss on an existing Secret — is tolerated and
// reported as created=false.
func EnsureTokenSecret(ctx context.Context, reader client.Reader, writer client.Writer, namespace, name string) (created bool, err error) {
	var existing corev1.Secret
	switch err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing); {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("get %s secret: %w", name, err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return false, fmt.Errorf("generate token: %w", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{TokenSecretKey: []byte(hex.EncodeToString(buf))},
	}
	adoptguard.WithAdoptedLabel(sec)
	if err := writer.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("create %s secret: %w", name, err)
	}
	return true, nil
}
