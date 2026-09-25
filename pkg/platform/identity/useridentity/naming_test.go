package useridentity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNameForSubject(t *testing.T) {
	const subject = "user:YWxpY2VAZXhhbXBsZS5jb20="
	n := NameForSubject(subject)

	assert.True(t, len(n) >= 3 && len(n) <= 63, "name must be a valid DNS label length, got %d", len(n))
	assert.Regexp(t, `^[a-z0-9-]+$`, n, "name must be DNS-safe lowercase")
	assert.Equal(t, n, NameForSubject(subject), "must be deterministic")
	assert.NotEqual(t, n, NameForSubject("user:Ym9iQGV4YW1wbGUuY29t"), "distinct subjects → distinct names")
}

func TestMasterSecretName(t *testing.T) {
	got := MasterSecretName("u-abc123", "linear-oauth")
	assert.Equal(t, "u-abc123-linear-oauth", got)
}

func TestIdPIdentitySecretName_DeterministicAndSuffixed(t *testing.T) {
	const subject = "user:YWxpY2VAZXhhbXBsZS5jb20="
	n := IdPIdentitySecretName(subject)
	assert.Equal(t, NameForSubject(subject)+"-idp-identity", n, "must equal base name + suffix")
	assert.True(t, strings.HasPrefix(n, "u-"), "must start with u- (same hash prefix as UserIdentity name)")
	// Determinism: two calls for the same subject must agree.
	assert.Equal(t, n, IdPIdentitySecretName(subject), "must be deterministic")
	// Distinct subjects must yield distinct names.
	assert.NotEqual(t, n, IdPIdentitySecretName("user:Ym9iQGV4YW1wbGUuY29t"), "distinct subjects → distinct names")
}
