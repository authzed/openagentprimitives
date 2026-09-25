package agentcmd

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// loadPrivateKeySigner reads an ECDSA private key from a PEM file at path and
// returns it as a crypto.Signer (an *ecdsa.PrivateKey satisfies the
// interface directly, so `oap agent sign` never needs to know the concrete
// type). Two encodings are tried, matching what OpenSSL and Go's own
// x509.MarshalPKCS8PrivateKey / MarshalECPrivateKey produce for the same
// key: PKCS#8 ("PRIVATE KEY") first, falling back to SEC1 ("EC PRIVATE
// KEY") if that fails to parse.
func loadPrivateKeySigner(path string) (crypto.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM file (no PEM block found)", path)
	}

	if key, pkcs8Err := x509.ParsePKCS8PrivateKey(block.Bytes); pkcs8Err == nil {
		ecKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: PKCS#8 key is %T, not an ECDSA private key", path, key)
		}
		return ecKey, nil
	}
	ecKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: PEM block is neither a valid PKCS#8 nor SEC1 EC private key: %w", path, err)
	}
	return ecKey, nil
}

// loadPublicKeyPEM reads an ECDSA public key from a PEM file at path
// (PKIX/SubjectPublicKeyInfo — the "PUBLIC KEY" format both `openssl ec
// -pubout` and x509.MarshalPKIXPublicKey produce).
func loadPublicKeyPEM(path string) (crypto.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM file (no PEM block found)", path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: not a valid PKIX public key: %w", path, err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: public key is %T, not an ECDSA public key", path, pub)
	}
	return ecPub, nil
}
