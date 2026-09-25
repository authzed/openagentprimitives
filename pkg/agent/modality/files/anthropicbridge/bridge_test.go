package anthropicbridge_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/modality/files/anthropicbridge"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

// fakeFC is a no-network stand-in for anthropicbridge.FilesClient, letting the
// bridge's own logic (store <-> provider byte-shuttling) be unit-tested
// without hitting the real Anthropic Files API.
type fakeFC struct {
	downloadData []byte
	uploadedID   string
	gotUpload    []byte
	gotName      string
}

func (f *fakeFC) Download(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.downloadData)), nil
}

func (f *fakeFC) Upload(_ context.Context, name string, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	f.gotName = name
	f.gotUpload = b
	return f.uploadedID, nil
}

func TestBridge_IntoStore(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	fc := &fakeFC{downloadData: []byte("filebytes")}
	b := anthropicbridge.New(store, fc)

	ref, err := b.IntoStore(ctx, "file_123")
	require.NoError(t, err)
	require.NotEmpty(t, ref)

	rc, err := store.Get(ctx, ref)
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "filebytes", string(got))
}

func TestBridge_IntoContainer(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMem()
	ref, err := store.Put(ctx, "ns/sess/report.html", bytes.NewReader([]byte("outbytes")))
	require.NoError(t, err)

	fc := &fakeFC{uploadedID: "file_out"}
	b := anthropicbridge.New(store, fc)

	id, err := b.IntoContainer(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, "file_out", id)
	assert.Equal(t, "outbytes", string(fc.gotUpload))
	assert.Equal(t, "report.html", fc.gotName)
}
