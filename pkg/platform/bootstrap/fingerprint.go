package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// MintSetFingerprint is a deterministic digest of WHAT MintAllInternalSecrets
// mints: every Secret name and every NATS grant subject. oap render folds it
// into the bootstrap Job's name, so an upgrade that adds a Secret or widens a
// grant yields a new Job object (and so a re-run) even when the operator image
// ref did not change — a mutable tag under --no-digest-pin. A pure function of
// compiled-in values: no time, no randomness.
func MintSetFingerprint() string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	for _, n := range MemoryTokenSecretNames {
		write(n)
	}
	write(AdmindTokenSecretName)
	write(v1alpha1.PassthroughLinkSigningKeySecret)
	write("spicebox-nats-identity")
	write("spicebox-nats-tls")
	for _, c := range natsClientCreds() {
		write(c.secretName)
		write(c.grant.Name)
		for _, s := range c.grant.PubAllow {
			write("pub:" + s)
		}
		for _, s := range c.grant.SubAllow {
			write("sub:" + s)
		}
	}
	write(postgresTokenSecretName)
	write(spiceDBTokenSecretName)
	return hex.EncodeToString(h.Sum(nil))
}
