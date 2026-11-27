package installcmd

import (
	"crypto/sha256"
	"encoding/hex"
)

// contentAddressedName names a Job by what it runs: base plus the first 10 hex
// characters of a SHA-256 over parts (NUL-separated, so two parts cannot blur
// into one).
//
// A Job's spec.template is immutable once applied. Under a fixed name, any
// template change makes the next apply — an `oap install` re-run or an Argo CD /
// Flux sync — fail with "field is immutable" against the Job the previous
// version left behind. Hashing the template into the name makes a changed
// template a NEW object instead, while identical inputs hash to the same name,
// so a byte-identical re-apply stays a server-side-apply no-op.
func contentAddressedName(base string, parts ...[]byte) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write(p)
	}
	return base + "-" + hex.EncodeToString(h.Sum(nil))[:10]
}
