package agentcmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
)

// writeECDSAPrivateKeyPEM generates an ECDSA-P256 key and writes it to dir as
// a PEM file using either PKCS#8 or SEC1 encoding, returning the path and the
// key (so callers can cross-check the loaded-back key matches).
func writeECDSAPrivateKeyPEM(t *testing.T, dir, name string, pkcs8 bool) (string, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	var der []byte
	var blockType string
	if pkcs8 {
		der, err = x509.MarshalPKCS8PrivateKey(priv)
		blockType = "PRIVATE KEY"
	} else {
		der, err = x509.MarshalECPrivateKey(priv)
		blockType = "EC PRIVATE KEY"
	}
	require.NoError(t, err)

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	return aptest.WriteFile(t, dir, name, string(pemBytes)), priv
}

func writeECDSAPublicKeyPEM(t *testing.T, dir, name string, pub *ecdsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return aptest.WriteFile(t, dir, name, string(pemBytes))
}

func TestLoadPrivateKeySigner_PKCS8(t *testing.T) {
	dir := t.TempDir()
	path, priv := writeECDSAPrivateKeyPEM(t, dir, "key.pem", true)

	signer, err := loadPrivateKeySigner(path)
	require.NoError(t, err)

	ecSigner, ok := signer.(*ecdsa.PrivateKey)
	require.True(t, ok, "loaded signer must be *ecdsa.PrivateKey, got %T", signer)
	assert.True(t, priv.Equal(ecSigner), "loaded key must equal the generated key")
}

func TestLoadPrivateKeySigner_SEC1(t *testing.T) {
	dir := t.TempDir()
	path, priv := writeECDSAPrivateKeyPEM(t, dir, "key.pem", false)

	signer, err := loadPrivateKeySigner(path)
	require.NoError(t, err)

	ecSigner, ok := signer.(*ecdsa.PrivateKey)
	require.True(t, ok, "loaded signer must be *ecdsa.PrivateKey, got %T", signer)
	assert.True(t, priv.Equal(ecSigner), "loaded key must equal the generated key")
}

func TestLoadPrivateKeySigner_MissingFile(t *testing.T) {
	_, err := loadPrivateKeySigner(filepath.Join(t.TempDir(), "does-not-exist.pem"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist.pem")
}

func TestLoadPrivateKeySigner_NotPEM(t *testing.T) {
	dir := t.TempDir()
	path := aptest.WriteFile(t, dir, "garbage.pem", "this is not a PEM file at all")

	_, err := loadPrivateKeySigner(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a PEM file")
}

func TestLoadPrivateKeySigner_WrongKeyType(t *testing.T) {
	dir := t.TempDir()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := aptest.WriteFile(t, dir, "rsa.pem", string(pemBytes))

	_, err = loadPrivateKeySigner(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an ECDSA private key")
}

func TestLoadPublicKeyPEM_Valid(t *testing.T) {
	dir := t.TempDir()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	path := writeECDSAPublicKeyPEM(t, dir, "key.pub", &priv.PublicKey)

	pub, err := loadPublicKeyPEM(path)
	require.NoError(t, err)

	ecPub, ok := pub.(*ecdsa.PublicKey)
	require.True(t, ok, "loaded public key must be *ecdsa.PublicKey, got %T", pub)
	assert.True(t, priv.PublicKey.Equal(ecPub), "loaded key must equal the generated public key")
}

func TestLoadPublicKeyPEM_MissingFile(t *testing.T) {
	_, err := loadPublicKeyPEM(filepath.Join(t.TempDir(), "does-not-exist.pub"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist.pub")
}

func TestLoadPublicKeyPEM_NotPEM(t *testing.T) {
	dir := t.TempDir()
	path := aptest.WriteFile(t, dir, "garbage.pub", "this is not a PEM file at all")

	_, err := loadPublicKeyPEM(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a PEM file")
}

func TestLoadPublicKeyPEM_WrongKeyType(t *testing.T) {
	dir := t.TempDir()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	path := aptest.WriteFile(t, dir, "rsa.pub", string(pemBytes))

	_, err = loadPublicKeyPEM(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an ECDSA public key")
}
