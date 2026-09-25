package files_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/modality/files"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// fakeBridge is a no-network stand-in for modality.Bridge, letting
// mount_artifact's own logic be unit-tested without a real provider.
type fakeBridge struct {
	intoContainerID  string
	intoContainerErr error
	gotRef           artifactstore.Ref
}

func (f *fakeBridge) IntoStore(context.Context, string) (artifactstore.Ref, error) {
	return "", nil
}

func (f *fakeBridge) IntoContainer(_ context.Context, ref artifactstore.Ref) (string, error) {
	f.gotRef = ref
	if f.intoContainerErr != nil {
		return "", f.intoContainerErr
	}
	return f.intoContainerID, nil
}

func TestMountArtifactPreloadsIntoContainer(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBridge{intoContainerID: "file_out"}
	tl := files.NewMountArtifact(fb)

	assert.Equal(t, "mount_artifact", tl.Name())
	assert.Equal(t, tool.KindMeta, tl.Kind())

	args, err := json.Marshal(map[string]any{"handle": "mem://ns/sess/x"})
	require.NoError(t, err, "marshal args")

	res, err := tl.Execute(ctx, args, &tool.SessionContext{})
	require.NoError(t, err, "Execute must not return a Go error")
	require.False(t, res.IsError, "want success, got: %s", res.Content)
	require.NotNil(t, res.ContainerUpload, "ContainerUpload must be set on success")
	assert.Equal(t, "file_out", res.ContainerUpload.FileID)
	assert.Equal(t, artifactstore.Ref("mem://ns/sess/x"), fb.gotRef, "bridge must receive the requested handle")
}

func TestMountArtifactNilBridgeIsError(t *testing.T) {
	ctx := context.Background()
	tl := files.NewMountArtifact(nil)

	args, err := json.Marshal(map[string]any{"handle": "mem://ns/sess/x"})
	require.NoError(t, err, "marshal args")

	res, err := tl.Execute(ctx, args, &tool.SessionContext{})
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "nil bridge must produce IsError")
}

func TestMountArtifactMissingHandleIsError(t *testing.T) {
	ctx := context.Background()
	tl := files.NewMountArtifact(&fakeBridge{})

	args, err := json.Marshal(map[string]any{})
	require.NoError(t, err, "marshal args")

	res, err := tl.Execute(ctx, args, &tool.SessionContext{})
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "empty handle must produce IsError")
}

func TestMountArtifactBridgeErrorIsError(t *testing.T) {
	ctx := context.Background()
	tl := files.NewMountArtifact(&fakeBridge{intoContainerErr: assertErr("boom")})

	args, err := json.Marshal(map[string]any{"handle": "mem://ns/sess/x"})
	require.NoError(t, err, "marshal args")

	res, err := tl.Execute(ctx, args, &tool.SessionContext{})
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "bridge error must produce IsError")
	assert.Contains(t, res.Content, "boom")
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
