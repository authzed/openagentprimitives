package channelevents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// DeliveryOperation identifies one reply across publisher retries and process
// restarts. It is correlation metadata, not authorization or a delivery receipt.
// A receiver must authenticate the envelope and bind SessionUID to the current
// session before using it. A recreated session or a new tool call is a different
// operation even when the reply text is identical.
type DeliveryOperation struct {
	ID            string `json:"id"`
	SessionUID    string `json:"sessionUID"`
	ToolUseID     string `json:"toolUseID"`
	PayloadDigest string `json:"payloadDigest"`
}

func replyOperationID(uid, toolUseID string) string {
	// Length prefixes keep arbitrary identity strings unambiguous.
	sum := sha256.Sum256([]byte(fmt.Sprintf("oap.reply.v1:%d:%s%d:%s", len(uid), uid, len(toolUseID), toolUseID)))
	return "reply-" + hex.EncodeToString(sum[:])
}

// NewDeliveryOperation derives its ID from identities already retained in the
// session and the assistant's durable tool-use record. The body digest pins the
// exact wire payload; it deliberately excludes timestamps and this descriptor.
func NewDeliveryOperation(uid, toolUseID string, p OutboundUserMessagePayload) (*DeliveryOperation, error) {
	if uid == "" || toolUseID == "" {
		return nil, fmt.Errorf("delivery operation requires session UID and tool-use ID")
	}
	p.Delivery = nil
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("delivery payload digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return &DeliveryOperation{ID: replyOperationID(uid, toolUseID), SessionUID: uid, ToolUseID: toolUseID, PayloadDigest: hex.EncodeToString(sum[:])}, nil
}

func (p OutboundUserMessagePayload) validateDelivery() error {
	if p.Delivery == nil {
		return nil // Older producers and framework notices have no tool call.
	}
	expected, err := NewDeliveryOperation(p.Delivery.SessionUID, p.Delivery.ToolUseID, p)
	if err != nil {
		return err
	}
	if *expected != *p.Delivery {
		return fmt.Errorf("delivery operation identity or payload digest mismatch")
	}
	return nil
}
