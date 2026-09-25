package memory

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID generates a unique ID for an Entry of Kind k: k.IDPrefix() plus 8
// random bytes hex-encoded, so it satisfies Put's prefix validation by
// construction.
func NewID(k Kind) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read does not fail in practice; if it does, the process
		// is unrecoverable and panicking is the honest answer.
		panic("memory.NewID: crypto/rand.Read failed: " + err.Error())
	}
	return k.IDPrefix() + hex.EncodeToString(b[:])
}
