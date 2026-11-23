package browser

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

// NewDeliveryReceiver accepts into the durable private browser transcript,
// independently of live tabs. The caller supplies component signing memory.
func (*Kind) NewDeliveryReceiver(mem memory.Memory) delivery.Receiver {
	return &transcriptReceiver{mem: mem}
}

type transcriptReceiver struct{ mem memory.Memory }

func (*transcriptReceiver) RetryAbsent() bool { return true }
func replyScope(i delivery.Intent) memory.Scope {
	return memory.Scope{Kind: "session", ID: i.Session.Namespace + "/" + i.Session.Name}
}

func replyID(i delivery.Intent) string { return replydelivery.IDPrefix + i.Payload.Delivery.ID }

func (r *transcriptReceiver) Lookup(ctx context.Context, i delivery.Intent) (*delivery.Receipt, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if r.mem == nil {
		return nil, fmt.Errorf("browser delivery memory unavailable")
	}
	result, err := r.mem.Query(ctx, memory.Query{Scope: replyScope(i), Kinds: []string{replydelivery.KindName}, IDs: []string{replyID(i)}})
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
		return nil, fmt.Errorf("browser receipt requires operator provenance")
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

func (r *transcriptReceiver) Accept(ctx context.Context, i delivery.Intent) (delivery.Receipt, error) {
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
	receipt := delivery.Receipt{OperationID: op.ID, SessionUID: op.SessionUID, PayloadDigest: op.PayloadDigest, DestinationDigest: digest, Transport: "browser-transcript", Reference: replyID(i), AcceptedAt: time.Now().UTC()}
	raw, err := json.Marshal(replydelivery.Content{Intent: i, Receipt: receipt})
	if err != nil {
		return delivery.Receipt{}, err
	}
	_, err = r.mem.Put(ctx, memory.Entry{Scope: replyScope(i), Kind: replydelivery.KindName, ID: replyID(i), CreatedAt: i.CreatedAt, Content: raw})
	if errors.Is(err, memory.ErrAppendOnlyConflict) {
		// Another worker may have accepted the exact same intent. Read the
		// winner back rather than swallowing a changed destination/body.
		winner, readErr := r.Lookup(ctx, i)
		if readErr != nil {
			return delivery.Receipt{}, readErr
		}
		if winner == nil {
			return delivery.Receipt{}, fmt.Errorf("browser receipt conflict without stored acceptance: %w", err)
		}
		return *winner, nil
	}
	if err != nil {
		return delivery.Receipt{}, err
	}
	return receipt, nil
}

func (r *transcriptReceiver) Close(ctx context.Context, i delivery.Intent) (*delivery.Receipt, error) {
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
	_, err = r.mem.Put(ctx, memory.Entry{Scope: replyScope(i), Kind: replydelivery.KindName, ID: replyID(i), CreatedAt: i.CreatedAt, Content: raw})
	if errors.Is(err, memory.ErrAppendOnlyConflict) {
		existing, readErr := r.Lookup(ctx, i)
		if errors.Is(readErr, delivery.ErrClosed) {
			return nil, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		if existing == nil {
			return nil, fmt.Errorf("browser close conflict without stored decision: %w", err)
		}
		return existing, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}
