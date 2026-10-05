package goals

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalevent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Publisher drains transactional audit intent. It publishes each saved envelope
// before signing another entry in that scope, preserving chain order on restart.
type Publisher struct {
	Store  domain.Store
	Memory memory.Memory
	Signer *provenance.Signer
	Notify func(context.Context, domain.Event) error
	mu     sync.Mutex
}

func (p *Publisher) NeedLeaderElection() bool { return true }
func (p *Publisher) Start(ctx context.Context) error {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if err := p.Flush(ctx); err != nil {
			log.FromContext(ctx).Info("goal audit publication pending", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}
func (p *Publisher) Flush(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	events, err := p.Store.Pending(ctx, 100)
	if err != nil {
		return err
	}
	for _, event := range events {
		scope, err := memory.ResourceScope(ResourceType, event.Goal.Domain.ID())
		if err != nil {
			return err
		}
		raw, err := p.Store.Envelope(ctx, event.ID)
		if err != nil {
			return err
		}
		var entry memory.Entry
		if len(raw) == 0 {
			content, err := json.Marshal(event)
			if err != nil {
				return err
			}
			entry = memory.Entry{Scope: scope, Kind: goalevent.KindName, ID: event.ID, CreatedAt: event.Goal.UpdatedAt.UTC().Truncate(time.Microsecond), Content: content}
			if event.OccurredAt != nil {
				entry.CreatedAt = event.OccurredAt.UTC().Truncate(time.Microsecond)
			}
			if err := p.Signer.EnsureSeeded(ctx, p.Memory, scope); err != nil {
				return err
			}
			if err := p.Signer.Sign(&entry); err != nil {
				return err
			}
			raw, err = json.Marshal(entry)
			if err != nil {
				p.Signer.InvalidateSeed(scope)
				return err
			}
			if err := p.Store.SaveEnvelope(ctx, event.ID, raw); err != nil {
				p.Signer.InvalidateSeed(scope)
				return err
			}
		} else {
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			if entry.ID != event.ID || entry.Scope != scope {
				return fmt.Errorf("goal audit envelope mismatch: %s", event.ID)
			}
		}
		if _, err := p.Memory.Put(ctx, entry); err != nil {
			p.Signer.InvalidateSeed(scope)
			return err
		}
		// The saved envelope may belong to a previous operator key. Re-seed from
		// what actually landed before signing the next event in this scope.
		p.Signer.InvalidateSeed(scope)
		if p.Notify != nil {
			if err := p.Notify(ctx, event); err != nil {
				return err
			}
		}
		if err := p.Store.Published(ctx, event.ID); err != nil {
			return err
		}
	}
	return nil
}
