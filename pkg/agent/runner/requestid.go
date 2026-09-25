package runner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newRequestID returns a 32-character hex string from 16 random bytes.
// Used as the correlation ID for KindToolApprovalRequest envelopes —
// echoed back on the matching KindToolApprovalApplied so the runner's
// approval Orchestrator can route the decision to the awaiting goroutine.
// Panics on a crypto/rand failure: a runner that can't generate random
// correlation IDs is already in deep trouble.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("runner: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}
