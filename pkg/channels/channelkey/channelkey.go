// Package channelkey holds the canonical channel-key → label-value function.
//
// Kept tiny + dependency-free so any package can import it without introducing
// cycles. There must be exactly ONE such function in the codebase: independent
// SHA-256 truncations drift silently, and a drift routes inbound messages to a
// brand-new bogus session instead of the intended one.
package channelkey

import (
	"crypto/sha256"
	"encoding/hex"
)

// LabelValue returns the value stamped on an AgentSession's LabelChannelKey
// (and LabelOutputChannelKey) label: a 63-char prefix of the key's SHA-256 hex
// digest. 63 is the Kubernetes label-value limit; 63 hex chars (252 bits) stay
// collision-resistant for all practical purposes.
func LabelValue(channelKey string) string {
	h := sha256.Sum256([]byte(channelKey))
	return hex.EncodeToString(h[:])[:63]
}
