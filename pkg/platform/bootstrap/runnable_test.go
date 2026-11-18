package bootstrap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
)

// fakeSchema replays readErrs in order (then nil), serving live as the schema.
type fakeSchema struct {
	live     string
	readErrs []error
	reads    int
	writes   []string
}

func (f *fakeSchema) ReadSchema(context.Context) (string, error) {
	f.reads++
	if len(f.readErrs) > 0 {
		err := f.readErrs[0]
		f.readErrs = f.readErrs[1:]
		if err != nil {
			return "", err
		}
	}
	return f.live, nil
}

func (f *fakeSchema) WriteSchema(_ context.Context, text string) error {
	f.writes = append(f.writes, text)
	f.live = text
	return nil
}

func fastBackoff(t *testing.T) {
	t.Helper()
	orig := SeedBackoff
	SeedBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 1}
	t.Cleanup(func() { SeedBackoff = orig })
}

const scaffold = "definition user {}"

func TestRunBootstrap(t *testing.T) {
	composed := "use expiration\n\ndefinition user {}\n\ndefinition mcp_github/repo {\n  relation grant_reader: user\n}"
	notFound := status.Error(codes.NotFound, "No schema has been defined; please call WriteSchema to start")

	cases := []struct {
		name       string
		schema     *fakeSchema
		wantWrites []string
		wantLive   string
	}{
		{
			name:       "empty datastore (NotFound): seeds the scaffold once",
			schema:     &fakeSchema{readErrs: []error{notFound}},
			wantWrites: []string{scaffold},
			wantLive:   scaffold,
		},
		{
			name:     "guardian-composed schema present: not replaced",
			schema:   &fakeSchema{live: composed},
			wantLive: composed,
		},
		{
			name:     "transient read errors: retried, then composed schema left alone",
			schema:   &fakeSchema{live: composed, readErrs: []error{status.Error(codes.Unavailable, "connection refused"), errors.New("dial timeout")}},
			wantLive: composed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastBackoff(t)
			require.NoError(t, RunBootstrap(context.Background(), logr.Discard(), NewSerializedSchemaIO(tc.schema), scaffold))
			assert.Equal(t, tc.wantWrites, tc.schema.writes)
			assert.Equal(t, tc.wantLive, tc.schema.live)
		})
	}
}

func TestRunBootstrap_ReturnsNilWhenContextEndsWhileSpiceDBDown(t *testing.T) {
	fastBackoff(t)
	down := make([]error, 1000)
	for i := range down {
		down[i] = status.Error(codes.Unavailable, "connection refused")
	}
	sch := &fakeSchema{readErrs: down}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	assert.NoError(t, RunBootstrap(ctx, logr.Discard(), NewSerializedSchemaIO(sch), scaffold), "an unreachable SpiceDB must never stop the manager")
	assert.Greater(t, sch.reads, 1, "retried while SpiceDB was down")
	assert.Empty(t, sch.writes)
}

// gatedSchema is a SchemaIO whose ReadSchema signals readStarted and then
// blocks until release is closed, so a test can attempt a concurrent write
// between the seed's read and its write.
type gatedSchema struct {
	mu          sync.Mutex
	live        string
	writes      []string
	readStarted chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (g *gatedSchema) ReadSchema(context.Context) (string, error) {
	g.once.Do(func() {
		close(g.readStarted)
		<-g.release
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live == "" {
		return "", status.Error(codes.NotFound, "No schema has been defined")
	}
	return g.live, nil
}

func (g *gatedSchema) WriteSchema(_ context.Context, text string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writes = append(g.writes, text)
	g.live = text
	return nil
}

// TestRunBootstrap_ConcurrentGuardianWriteIsNotOverwritten drives the race
// interleaving: the seed reads an empty datastore, the guardian writes its
// composition, then the seed would write the bare scaffold over it. With the
// shared SerializedSchemaIO the guardian's write waits for the seed's
// read-then-write to finish, so the composition is the last write and wins.
func TestRunBootstrap_ConcurrentGuardianWriteIsNotOverwritten(t *testing.T) {
	composed := "definition user {}\n\ndefinition mcp_github/repo {\n  relation grant_reader: user\n}"
	inner := &gatedSchema{readStarted: make(chan struct{}), release: make(chan struct{})}
	shared := NewSerializedSchemaIO(inner)

	seedDone := make(chan error, 1)
	go func() { seedDone <- RunBootstrap(context.Background(), logr.Discard(), shared, scaffold) }()
	<-inner.readStarted // the seed has read and is about to write

	guardianDone := make(chan error, 1)
	go func() { guardianDone <- shared.WriteSchema(context.Background(), composed) }()
	select {
	case err := <-guardianDone:
		t.Fatalf("guardian write landed inside the seed's read-then-write (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(inner.release)
	require.NoError(t, <-seedDone)
	require.NoError(t, <-guardianDone)
	assert.Equal(t, []string{scaffold, composed}, inner.writes, "the seed's scaffold lands first; the guardian's composition is last")
	assert.Equal(t, composed, inner.live, "the composed schema survives")
}

func TestSerializedSchemaIO_WriteHonoursContextWhileGateHeld(t *testing.T) {
	inner := &gatedSchema{readStarted: make(chan struct{}), release: make(chan struct{})}
	shared := NewSerializedSchemaIO(inner)
	go func() { _, _ = shared.seedIfEmpty(context.Background(), scaffold) }()
	<-inner.readStarted
	t.Cleanup(func() { close(inner.release) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, shared.WriteSchema(ctx, "definition user {}"), context.DeadlineExceeded, "a write waiting on the gate gives up with its context")
}
