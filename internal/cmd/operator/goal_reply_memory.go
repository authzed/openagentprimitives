package main

import (
	"context"
	"github.com/authzed/openagentprimitives/pkg/memory"
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
	return m.Writer.Put(ctx, entry)
}
