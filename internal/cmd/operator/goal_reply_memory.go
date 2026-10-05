package main

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/shadow"
)

// goalReplyMemory reads the durable goal backend, including when shadow memory
// reads are configured to use the ephemeral primary. Writes use the operator's
// existing signing facade: two wrappers around the same signer must not race
// Sign+Put on the same session's operator audit chain.
type goalReplyMemory struct {
	memory.Memory
	Writer memory.Memory
}

func (m *goalReplyMemory) Put(ctx context.Context, entry memory.Entry) (memory.Entry, error) {
	return m.Writer.Put(shadow.WithReadFrom(ctx, shadow.ReadFromSecondary), entry)
}

// auditSeedMemory always recovers operator chains from the durable shadow half,
// even when ordinary reads use the ephemeral primary after process restart.
// Backends without shadow memory ignore this context override.
type auditSeedMemory struct{ memory.Memory }

func (m auditSeedMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	return m.Memory.Query(shadow.WithReadFrom(ctx, shadow.ReadFromSecondary), q)
}
