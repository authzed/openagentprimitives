package fake_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

// The gate's third leg. Without this assertion holding, every bronzethread
// bundle carrying a file silently takes the gate-closed path no matter how the
// Channel and AgentClass are configured.
func TestFakeKindImplementsAttachmentFetcher(t *testing.T) {
	var k any = &fake.Kind{}
	_, ok := k.(channelkinds.AttachmentFetcher)
	assert.True(t, ok, "bronzethread bundles cannot carry attachments unless the fake kind can fetch")
}

func TestFetchAttachmentWithoutSourceErrors(t *testing.T) {
	fake.ResetAttachmentSource()
	_, err := fake.Kind{}.FetchAttachment(context.Background(), channelkinds.Deps{}, "missing")
	require.Error(t, err, "a missing source must not read as a successful 0-byte fetch")
	assert.Contains(t, err.Error(), "missing", "the error names which id had nothing behind it")
}

func TestFetchAttachmentServesTheInstalledSource(t *testing.T) {
	fake.SetAttachmentSource(func(externalID string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("bytes for " + externalID)), nil
	})
	t.Cleanup(fake.ResetAttachmentSource)

	rc, err := fake.Kind{}.FetchAttachment(context.Background(), channelkinds.Deps{}, "f1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Close() })

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "bytes for f1", string(got))
}

func TestAttachmentBoundsAreGenerousEnoughToLetThePipelineClamp(t *testing.T) {
	b := fake.Kind{}.AttachmentBounds()
	assert.GreaterOrEqual(t, b.MaxSizeBytes, int64(25<<20),
		"a stingy fake bound would mask the pipeline's own clamping, which is what the tests are for")
	assert.Positive(t, b.MaxPerMessage)
}
