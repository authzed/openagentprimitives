package goals

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalinmem "github.com/authzed/openagentprimitives/pkg/memory/goals/inmem"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/stretchr/testify/require"
)

type blockedConsentMemory struct {
	memory.Memory
	entered chan struct{}
	release chan struct{}
}

func (m *blockedConsentMemory) Put(ctx context.Context, entry memory.Entry) (memory.Entry, error) {
	if entry.ID == "goalconsent-request-review" {
		close(m.entered)
		<-m.release
		return memory.Entry{}, errors.New("injected consent write failure")
	}
	return m.Memory.Put(ctx, entry)
}

func TestConsentUsesSharedWriterAcrossFailedConcurrentAppend(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	mem := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	blocked := &blockedConsentMemory{Memory: mem, entered: make(chan struct{}), release: make(chan struct{})}
	writer := provenance.NewSigningMemory(blocked, signer)
	now := time.Now().UTC().Truncate(time.Microsecond)
	g := domain.Goal{ID: "goal-test", Domain: domain.Domain{Namespace: "team", Owner: "YWxpY2VAZXhhbXBsZS5jb20", Class: "assistant", ClassUID: "uid"}, Revision: 3, Title: "Reminder", Outcome: "Stretch", Execution: &domain.ExecutionConsent{ApprovalMode: "plan", Session: "team/session", Digest: "review", Terms: domain.ExecutionTerms{DueAt: now, ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{ApprovalSeconds: 90}}}}
	store := goalinmem.New()
	_, err := store.Commit(ctx, domain.Mutation{Goal: g, RequestID: "seed", Hash: "seed", Event: domain.Event{ID: "goalev-seed", Goal: g}})
	require.NoError(t, err)
	p := &ConsentPublisher{Service: &domain.Service{Store: store}, Memory: mem, Writer: writer}
	consentDone := make(chan error, 1)
	go func() { consentDone <- p.Notify(ctx, domain.Event{Action: "request_execution", Goal: g}) }()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("consent did not reach storage")
	}
	// Another operator append shares the signing facade. It must wait until
	// the failed consent invalidates its phantom chain position.
	appended := make(chan memory.Entry, 1)
	appendErr := make(chan error, 1)
	go func() {
		raw, err := json.Marshal(goalconsent.Content{Goal: g})
		if err != nil {
			appendErr <- err
			return
		}
		result, err := writer.Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: goalconsent.KindName, ID: "goalconsent-other", CreatedAt: now, Content: raw})
		appended <- result
		appendErr <- err
	}()
	select {
	case <-appended:
		t.Fatal("concurrent append escaped shared signing lock")
	case <-time.After(30 * time.Millisecond):
	}
	close(blocked.release)
	require.Error(t, <-consentDone)
	result := <-appended
	require.NoError(t, <-appendErr)
	require.Equal(t, uint64(1), result.Provenance.Seq)
	report := provenance.NewVerifier(keys).VerifyChain(signer.Publisher(), []memory.Entry{result}, nil)
	require.Empty(t, report.Findings)
}
