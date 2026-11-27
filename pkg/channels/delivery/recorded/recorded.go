// Package recorded is a delivery.Receiver whose acceptance is one
// operator-signed, append-only reply_delivery entry in the session's durable
// memory. A kind whose transport IS that record (the browser transcript, the
// in-process test kind) uses it rather than re-implementing the contract:
// lookup by delivery ID, accept-once, and a durable close that fences an
// in-flight acceptance.
package recorded

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/replydelivery"
)

// New returns a receiver that records acceptance in mem under transport, the
// name its receipts carry. The caller supplies component signing memory.
func New(mem memory.Memory, transport string) delivery.Receiver {
	return &receiver{mem: mem, transport: transport}
}

type receiver struct {
	mem       memory.Memory
	transport string
}

var _ delivery.Closer = (*receiver)(nil)

func (*receiver) RetryAbsent() bool { return true }

// Scope is the memory scope an intent's acceptance is recorded in: the
// replying session's own.
func Scope(i delivery.Intent) memory.Scope {
	return memory.Scope{Kind: "session", ID: i.Session.Namespace + "/" + i.Session.Name}
}

// ID is the entry ID of an intent's acceptance: its delivery ID.
func ID(i delivery.Intent) string { return replydelivery.IDPrefix + i.Payload.Delivery.ID }

func (r *receiver) Lookup(ctx context.Context, i delivery.Intent) (*delivery.Receipt, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if r.mem == nil {
		return nil, fmt.Errorf("%s delivery memory unavailable", r.transport)
	}
	result, err := r.mem.Query(ctx, memory.Query{Scope: Scope(i), Kinds: []string{replydelivery.KindName}, IDs: []string{ID(i)}})
	if err != nil {
		return nil, err
	}
	if len(result.Entries) == 0 {
		return nil, nil
	}
	if len(result.Entries) != 1 {
		return nil, delivery.ErrConflict
	}
	entry := result.Entries[0]
	if entry.Provenance == nil || entry.Provenance.Publisher != "system:operator" {
		return nil, fmt.Errorf("%s receipt requires operator provenance", r.transport)
	}
	var c replydelivery.Content
	if err := json.Unmarshal(entry.Content, &c); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(c.Intent, i) {
		return nil, delivery.ErrConflict
	}
	if c.Closed {
		return nil, delivery.ErrClosed
	}
	if err := c.Receipt.Validate(i); err != nil {
		return nil, err
	}
	return &c.Receipt, nil
}

func (r *receiver) Accept(ctx context.Context, i delivery.Intent) (delivery.Receipt, error) {
	if receipt, err := r.Lookup(ctx, i); err != nil {
		return delivery.Receipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}
	digest, err := i.Destination.Digest()
	if err != nil {
		return delivery.Receipt{}, err
	}
	op := i.Payload.Delivery
	receipt := delivery.Receipt{OperationID: op.ID, SessionUID: op.SessionUID, PayloadDigest: op.PayloadDigest, DestinationDigest: digest, Transport: r.transport, Reference: ID(i), AcceptedAt: time.Now().UTC()}
	raw, err := json.Marshal(replydelivery.Content{Intent: i, Receipt: receipt})
	if err != nil {
		return delivery.Receipt{}, err
	}
	_, err = r.mem.Put(ctx, memory.Entry{Scope: Scope(i), Kind: replydelivery.KindName, ID: ID(i), CreatedAt: i.CreatedAt, Content: raw})
	if errors.Is(err, memory.ErrAppendOnlyConflict) {
		// Another worker may have accepted the exact same intent. Read the
		// winner back rather than swallowing a changed destination/body.
		winner, readErr := r.Lookup(ctx, i)
		if readErr != nil {
			return delivery.Receipt{}, readErr
		}
		if winner == nil {
			return delivery.Receipt{}, fmt.Errorf("%s receipt conflict without stored acceptance: %w", r.transport, err)
		}
		return *winner, nil
	}
	if err != nil {
		return delivery.Receipt{}, err
	}
	return receipt, nil
}

func (r *receiver) Close(ctx context.Context, i delivery.Intent) (*delivery.Receipt, error) {
	existing, err := r.Lookup(ctx, i)
	if errors.Is(err, delivery.ErrClosed) {
		return nil, nil
	}
	if err != nil || existing != nil {
		return existing, err
	}
	raw, err := json.Marshal(replydelivery.Content{Intent: i, Closed: true})
	if err != nil {
		return nil, err
	}
	_, err = r.mem.Put(ctx, memory.Entry{Scope: Scope(i), Kind: replydelivery.KindName, ID: ID(i), CreatedAt: i.CreatedAt, Content: raw})
	if errors.Is(err, memory.ErrAppendOnlyConflict) {
		existing, readErr := r.Lookup(ctx, i)
		if errors.Is(readErr, delivery.ErrClosed) {
			return nil, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		if existing == nil {
			return nil, fmt.Errorf("%s close conflict without stored decision: %w", r.transport, err)
		}
		return existing, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}
