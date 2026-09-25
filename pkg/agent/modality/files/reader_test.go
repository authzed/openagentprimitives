package files_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

func TestStoreReaderReadRange(t *testing.T) {
	ctx := context.Background()
	st := blob.NewMem()

	// Put test data into the store
	ref, err := st.Put(ctx, "ns/sess/a", bytes.NewReader([]byte("0123456789")))
	require.NoError(t, err, "Put must succeed")

	r := files.StoreReader{Store: st}

	// Test: read full content (start=0, length=0 means to EOF)
	data, size, err := r.ReadRange(ctx, ref, 0, 0)
	require.NoError(t, err, "ReadRange full must succeed")
	assert.Equal(t, []byte("0123456789"), data, "ReadRange(0, 0) returns all data")
	assert.Equal(t, int64(10), size, "ReadRange(0, 0) returns size 10")

	// Test: read partial range (start=3, length=4)
	data, size, err = r.ReadRange(ctx, ref, 3, 4)
	require.NoError(t, err, "ReadRange partial must succeed")
	assert.Equal(t, []byte("3456"), data, "ReadRange(3, 4) returns bytes 3-6")
	assert.Equal(t, int64(4), size, "ReadRange(3, 4) returns size 4")

	// Test: start past EOF
	data, size, err = r.ReadRange(ctx, ref, 20, 5)
	require.NoError(t, err, "ReadRange past EOF must not error")
	assert.Equal(t, []byte{}, data, "ReadRange(20, 5) returns empty slice")
	assert.Equal(t, int64(0), size, "ReadRange(20, 5) returns size 0")
}

type fakeGetter struct {
	data []byte
}

func (f *fakeGetter) Get(ctx context.Context, ref string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func TestReaderFromClient(t *testing.T) {
	ctx := context.Background()

	r := files.ReaderFromClient(&fakeGetter{data: []byte("hello world")})

	// Test: read partial range (start=0, length=5)
	data, size, err := r.ReadRange(ctx, "any-ref", 0, 5)
	require.NoError(t, err, "ReadRange must succeed")
	assert.Equal(t, []byte("hello"), data, "ReadRange(0, 5) returns 'hello'")
	assert.Equal(t, int64(5), size, "ReadRange(0, 5) returns size 5")
}
