package passthroughlink

import (
	"bytes"
	"encoding/hex"
	"fmt"
)

// MinKeyLen is the minimum length, in bytes, of an HMAC signing key loaded
// from deployed key material. It names a contract the codebase already
// assumed but never enforced: New's documentation asks for "at least 32
// bytes", and `oap install` generates exactly 32 random bytes.
//
// The floor matters because HMAC-SHA256 under a short key is guessable and
// under a zero-length key is publicly computable — anyone can then forge an
// idd_session cookie for an arbitrary subject, or mint their own /content and
// artifact-view capability tokens.
const MinKeyLen = 32

// DecodeHexKey turns deployed key material — the raw bytes of a mounted
// Secret file or a Secret's "key" value — into usable HMAC key bytes, and is
// the single place the length floor is enforced.
//
// EVERY production reader of the spicebox-passthrough-link-key Secret must call
// this rather than restating trim-then-decode: three separate notions of "valid"
// is how a zero-length key became loadable in two of them.
//
// It rejects, in order: material that is empty or whitespace-only, material that
// is not valid hex, and a decoded key shorter than MinKeyLen. The empty case
// needs its own arm because hex.DecodeString("") returns ([]byte{}, nil) — a
// silent, publicly-computable zero-length key rather than an error.
func DecodeHexKey(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("signing key is empty")
	}
	key, err := hex.DecodeString(string(trimmed))
	if err != nil {
		// Deliberately does not wrap err. hex.InvalidByteError stringifies as
		// "encoding/hex: invalid byte: U+0041 'A'", which would echo a
		// character of the key itself into logs. The offending byte tells an
		// operator nothing they cannot get from "it is not valid hex", so the
		// position and value are both dropped.
		return nil, fmt.Errorf("signing key is not valid hex (expected %d hex characters)", MinKeyLen*2)
	}
	if len(key) < MinKeyLen {
		return nil, fmt.Errorf("signing key is too short: %d bytes, need at least %d", len(key), MinKeyLen)
	}
	return key, nil
}
