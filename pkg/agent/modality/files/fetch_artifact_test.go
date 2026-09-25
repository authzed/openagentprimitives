package files_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

type errReader struct{}

func (errReader) ReadRange(ctx context.Context, ref artifactstore.Ref, start, length int64) ([]byte, int64, error) {
	return nil, 0, artifactstore.ErrNotFound
}

func TestFetchArtifactReturnsSlice(t *testing.T) {
	ctx := context.Background()
	st := blob.NewMem()
	ref, err := st.Put(ctx, "ns/sess/a", bytes.NewReader([]byte("hello world")))
	require.NoError(t, err)

	tl := files.NewFetchArtifact(files.StoreReader{Store: st})

	assert.Equal(t, "fetch_artifact", tl.Name())
	assert.Equal(t, tool.KindMeta, tl.Kind())

	args := struct {
		Handle string `json:"handle"`
		Start  int64  `json:"start"`
		Length int64  `json:"length"`
	}{
		Handle: string(ref),
		Start:  0,
		Length: 5,
	}
	argsJSON, err := json.Marshal(args)
	require.NoError(t, err)

	res, err := tl.Execute(ctx, argsJSON, &tool.SessionContext{})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content, "hello")
}

func TestFetchArtifactUnknownHandleIsError(t *testing.T) {
	ctx := context.Background()

	tl := files.NewFetchArtifact(errReader{})

	args := struct {
		Handle string `json:"handle"`
	}{
		Handle: "mem://nope",
	}
	argsJSON, err := json.Marshal(args)
	require.NoError(t, err)

	res, err := tl.Execute(ctx, argsJSON, &tool.SessionContext{})
	require.NoError(t, err)
	assert.True(t, res.IsError)
}
