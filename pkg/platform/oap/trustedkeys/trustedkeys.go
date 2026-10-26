// Package trustedkeys embeds the public key that signs the example agents
// published under OAPAgentsPrefix, and decides which refs must verify against it.
package trustedkeys

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// OAPAgentsPrefix is the registry path whose refs must carry a valid
// signature from the embedded key.
const OAPAgentsPrefix = "ghcr.io/authzed/openagentprimitives/"

//go:embed oap-agents.pub
var oapAgentsPub []byte

func parseKey(raw []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("embedded oap-agents.pub: no PEM block found")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("embedded oap-agents.pub: not a valid PKIX public key: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("embedded oap-agents.pub: key is %T, not ECDSA", pub)
	}
	return ec, nil
}

// RequiredKey reports whether ref falls under OAPAgentsPrefix and, if so,
// returns the embedded key it must verify against. For refs outside the prefix
// it returns (nil, false, nil). Hosts are compared canonically through
// oci.RefHasPrefix, so "ghcr.io:443", "ghcr.io." and "GHCR.IO" are all pinned
// like "ghcr.io"; a host with no canonical form is an error, never an unpinned
// match. An unparseable ref is an error. If ref is under
// the prefix but the embedded key fails to parse, it returns pinned=true
// together with the error, so a caller that drops err still fails closed.
func RequiredKey(ref string) (pub crypto.PublicKey, pinned bool, err error) {
	pinned, err = oci.RefHasPrefix(ref, OAPAgentsPrefix)
	if err != nil {
		return nil, false, err
	}
	if !pinned {
		return nil, false, nil
	}
	pub, err = parseKey(oapAgentsPub)
	if err != nil {
		return nil, true, err
	}
	return pub, true, nil
}
