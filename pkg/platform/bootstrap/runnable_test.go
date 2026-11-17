package bootstrap

import (
	"context"
	"errors"
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
			require.NoError(t, RunBootstrap(context.Background(), logr.Discard(), tc.schema, scaffold))
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

	assert.NoError(t, RunBootstrap(ctx, logr.Discard(), sch, scaffold), "an unreachable SpiceDB must never stop the manager")
	assert.Greater(t, sch.reads, 1, "retried while SpiceDB was down")
	assert.Empty(t, sch.writes)
}
