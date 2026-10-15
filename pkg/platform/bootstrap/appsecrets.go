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

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// AdmindTokenSecretName is the admin API bearer token Secret. It is a plain
// random token like the memory tokens (minted via EnsureTokenSecret), so it has
// no dedicated minter — RunBootstrap mints it by name.
const AdmindTokenSecretName = "spicebox-admind-token"

// EnsurePassthroughLinkKey creates the passthrough-link HMAC signing-key Secret
// if absent, or repairs it if the existing `key` is unusable (not hex, too
// short) — the ESO/GitOps-provisioned-empty case. It reports minted=true when it
// wrote a new key (create OR repair), so the caller can roll the consumers
// (channelsd/webd) that read the key once at startup. An existing, usable key is
// left untouched (minted=false), so it is never rotated out from under a running
// signer, and a repair preserves the caller's other metadata (labels/annotations).
//
// logf narrates the outcome (a rotation is the one thing here an operator must be
// able to read afterwards); nil discards. reader must be a live/uncached reader.
func EnsurePassthroughLinkKey(ctx context.Context, reader client.Reader, writer client.Client, logf func(string, ...any)) (minted bool, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	name := v1alpha1.PassthroughLinkSigningKeySecret
	var existing corev1.Secret
	getErr := reader.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: name}, &existing)
	var unusable error
	switch {
	case getErr == nil:
		if _, unusable = passthroughlink.DecodeHexKey(existing.Data["key"]); unusable == nil {
			logf("  %s Secret exists with a usable key, skipping", name)
			return false, nil // usable key present — never rotate
		}
	case !apierrors.IsNotFound(getErr):
		return false, fmt.Errorf("get %s secret: %w", name, getErr)
	}

	encoded, err := randomHexKey()
	if err != nil {
		return false, err
	}

	if getErr == nil { // exists but unusable → repair in place, preserving metadata
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["key"] = encoded
		if err := writer.Update(ctx, &existing); err != nil {
			return false, fmt.Errorf("update %s secret: %w", name, err)
		}
		logf("  regenerated the key in the existing %s Secret (%v)", name, unusable)
		return true, nil
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: TokenSecretNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"key": encoded},
	}
	adoptguard.WithAdoptedLabel(sec)
	if err := writer.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("create %s secret: %w", name, err)
	}
	logf("  generated %s Secret", name)
	return true, nil
}

// randomHexKey returns 32 random bytes hex-encoded (64 chars) — the HMAC key
// shape passthroughlink.DecodeHexKey accepts.
func randomHexKey() ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	return []byte(hex.EncodeToString(buf)), nil
}
