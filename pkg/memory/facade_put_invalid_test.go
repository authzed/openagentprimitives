package memory_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// mutableTestKind is a plain (non-append-only) Kind so these cases exercise
// Put's validation arm without also crossing the provenance door.
type mutableTestKind struct {
	name, prefix string
}

func (k mutableTestKind) Name() string              { return k.name }
func (k mutableTestKind) IDPrefix() string          { return k.prefix }
func (mutableTestKind) Retention() memory.Retention { return memory.Retention{} }
func (mutableTestKind) ContentSchema() reflect.Type { return nil }
func (mutableTestKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (mutableTestKind) WriteAuthority() memory.WriteAuthority          { return memory.SessionWritten }
func (mutableTestKind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return noopHooks{} }

// putFaultBackend fails Put with a store-level fault.
type putFaultBackend struct {
	memory.Backend
	err error
}

func (b *putFaultBackend) Put(ctx context.Context, e memory.Entry) error {
	if b.err != nil {
		return b.err
	}
	return b.Backend.Put(ctx, e)
}

// TestPutValidationErrorsAreErrInvalidEntry pins the distinction the sentinel
// exists to draw: everything Put refuses on the ENTRY's own merits is
// ErrInvalidEntry (permanent — a retry cannot help), and a store fault is not
// (a retry usually does). httpsrv turns that distinction into 400 vs 5xx and
// httpclient turns 4xx vs 5xx into "give up" vs "retry", so mislabelling a fault
// here loses a durable write with no retry and no error the caller can act on.
func TestPutValidationErrorsAreErrInvalidEntry(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(mutableTestKind{name: "mk", prefix: "mk-"})
	memory.RegisterKind(mutableTestKind{name: "other", prefix: "other-"})
	scope := memory.Scope{Kind: "session", ID: "ns/n"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	cases := []struct {
		name    string
		entry   memory.Entry
		backend memory.Backend
		invalid bool // errors.Is(err, ErrInvalidEntry)
	}{
		{
			name:    "unregistered Kind: ErrInvalidEntry",
			entry:   memory.Entry{Scope: scope, Kind: "nosuchkind", ID: "x-1"},
			backend: inmem.NewBackend(),
			invalid: true,
		},
		{
			name:    "Entry.ID missing the Kind's prefix: ErrInvalidEntry",
			entry:   memory.Entry{Scope: scope, Kind: "mk", ID: "wrong-1"},
			backend: inmem.NewBackend(),
			invalid: true,
		},
		{
			name: "Link.ID missing the target Kind's prefix: ErrInvalidEntry",
			entry: memory.Entry{Scope: scope, Kind: "mk", ID: "mk-1",
				Links: []memory.Link{{Relation: "r", Kind: "other", ID: "mk-2"}}},
			backend: inmem.NewBackend(),
			invalid: true,
		},
		{
			name:    "store fault on the durable write: NOT ErrInvalidEntry",
			entry:   memory.Entry{Scope: scope, Kind: "mk", ID: "mk-1"},
			backend: &putFaultBackend{Backend: inmem.NewBackend(), err: errors.New("connection reset by peer")},
			invalid: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := memory.NewLocal(tc.backend).Put(ctx, tc.entry)
			require.Error(t, err, "Put must refuse this entry")
			assert.Equal(t, tc.invalid, errors.Is(err, memory.ErrInvalidEntry),
				"errors.Is(err, ErrInvalidEntry) for: %v", err)
		})
	}
}
