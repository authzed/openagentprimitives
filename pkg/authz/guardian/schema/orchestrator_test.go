package schema_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

type fakeIO struct {
	cur      string
	readErr  error
	written  string
	writeErr error
}

func (f *fakeIO) ReadSchema(_ context.Context) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.cur, nil
}

func (f *fakeIO) WriteSchema(_ context.Context, text string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written = text
	return nil
}

func TestRunWritesWhenChanged(t *testing.T) {
	io := &fakeIO{cur: baseSchema}
	pairs := []schema.GrantPair{{ResourceType: "github_repo", Permission: "admin"}}
	res, err := schema.Run(context.Background(), io, pairs)
	require.NoError(t, err, "Run")
	assert.True(t, res.Changed, "Changed=true expected")
	assert.NotEmpty(t, io.written, "WriteSchema must be called")
}

func TestRunSkipsWriteWhenNoChange(t *testing.T) {
	io := &fakeIO{cur: baseSchema}
	res, err := schema.Run(context.Background(), io, nil)
	require.NoError(t, err, "Run")
	assert.False(t, res.Changed, "Changed=false expected")
	assert.Empty(t, io.written, "WriteSchema must not be called when no change")
}

func TestRunSurfacesReadError(t *testing.T) {
	io := &fakeIO{readErr: errors.New("boom")}
	_, err := schema.Run(context.Background(), io, nil)
	require.Error(t, err, "ReadSchema failure must surface")
}
