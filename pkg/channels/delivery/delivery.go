// Package delivery defines transport acceptance, separately from publication
// and human observation. Receivers are supplied by registered channel kinds.
package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

var ErrConflict = errors.New("delivery identity or destination conflict")
var ErrClosed = errors.New("delivery operation durably closed without acceptance")

type Destination struct {
	Kind          string `json:"kind"`
	ChannelUID    string `json:"channelUID"`
	BindingDigest string `json:"bindingDigest"`
	Recipient     string `json:"recipient"`
}

type Intent struct {
	Session     channelevents.SessionRef                 `json:"session"`
	Destination Destination                              `json:"destination"`
	Payload     channelevents.OutboundUserMessagePayload `json:"payload"`
	CreatedAt   time.Time                                `json:"createdAt"`
}

func (i Intent) Validate() error {
	if i.Session.Namespace == "" || i.Session.Name == "" || i.Payload.Delivery == nil || i.CreatedAt.IsZero() || i.Destination.Kind == "" || i.Destination.ChannelUID == "" || i.Destination.BindingDigest == "" || i.Destination.Recipient == "" {
		return fmt.Errorf("delivery requires a bound session, destination and operation")
	}
	return i.Payload.Validate()
}

func (d Destination) Digest() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// Receipt establishes acceptance by the named transport only. It does not
// prove that the recipient opened, read or acted on the message.
type Receipt struct {
	OperationID       string    `json:"operationID"`
	SessionUID        string    `json:"sessionUID"`
	PayloadDigest     string    `json:"payloadDigest"`
	DestinationDigest string    `json:"destinationDigest"`
	Transport         string    `json:"transport"`
	Reference         string    `json:"reference"`
	AcceptedAt        time.Time `json:"acceptedAt"`
}

func (r Receipt) Validate(i Intent) error {
	if err := i.Validate(); err != nil {
		return err
	}
	digest, err := i.Destination.Digest()
	if err != nil {
		return err
	}
	op := i.Payload.Delivery
	if r.OperationID != op.ID || r.SessionUID != op.SessionUID || r.PayloadDigest != op.PayloadDigest || r.DestinationDigest != digest || r.Transport == "" || r.Reference == "" || r.AcceptedAt.IsZero() {
		return ErrConflict
	}
	return nil
}

// Receiver must reconcile an uncertain attempt before another send. Lookup
// returns nil for absence at the read, not proof that an in-flight send cannot
// subsequently commit. ErrClosed is a durable refusal of all future acceptance.
// RetryAbsent is true only when Accept itself is durably idempotent for the
// exact operation, body and destination. A live websocket enqueue cannot be
// an implementation of this contract.
type Receiver interface {
	Lookup(context.Context, Intent) (*Receipt, error)
	Accept(context.Context, Intent) (Receipt, error)
	RetryAbsent() bool
}

// Closer atomically closes an operation against all in-flight and future
// attempts. It returns an existing receipt if acceptance already won, or nil
// only after a durable rejection wins. Unsupported closure leaves effects
// unknown; an ordinary absent lookup is insufficient.
type Closer interface {
	Close(context.Context, Intent) (*Receipt, error)
}
