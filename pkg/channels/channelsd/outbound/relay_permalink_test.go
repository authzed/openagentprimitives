package outbound

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const testPermalink = "https://slack.test/archives/C01ABCDEF/p12345678"

// TestRelay_PatchOutputChannel_PersistsPermalink: the canonical thread permalink
// the sender captured on first root is written onto spec.outputChannel.external
// alongside thread_ts, both persisted and reflected in memory, so webd can read
// it off the session for the artifact-view origin link.
func TestRelay_PatchOutputChannel_PersistsPermalink(t *testing.T) {
	nc := connectNATS(t)
	sess := cronSession("cron-session", map[string]string{"channel_id": "C01ABCDEF"})
	cli := fakeClientWith(t, sess)
	r := &Relay{NC: nc, K8s: cli}

	err := r.patchOutputChannel(context.Background(), sess, map[string]string{
		"thread_ts": "1234.5678", "channel_id": "C01ABCDEF", "permalink": testPermalink,
	})
	require.NoError(t, err, "patchOutputChannel")

	assert.Equal(t, testPermalink, sess.Spec.OutputChannel.External["permalink"], "in-memory reflection carries permalink")

	var patched spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cron-session"}, &patched))
	assert.Equal(t, testPermalink, patched.Spec.OutputChannel.External["permalink"], "persisted permalink")
	assert.Equal(t, "1234.5678", patched.Spec.OutputChannel.External["thread_ts"], "thread_ts still persisted")
}

// TestRelay_PatchOutputChannel_NoPermalink_WritesNone: when the sender captured
// no permalink (getPermalink failed), no permalink key is written.
func TestRelay_PatchOutputChannel_NoPermalink_WritesNone(t *testing.T) {
	nc := connectNATS(t)
	sess := cronSession("cron-session", map[string]string{"channel_id": "C01ABCDEF"})
	cli := fakeClientWith(t, sess)
	r := &Relay{NC: nc, K8s: cli}

	err := r.patchOutputChannel(context.Background(), sess, map[string]string{
		"thread_ts": "1234.5678", "channel_id": "C01ABCDEF",
	})
	require.NoError(t, err, "patchOutputChannel")

	var patched spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cron-session"}, &patched))
	_, has := patched.Spec.OutputChannel.External["permalink"]
	assert.False(t, has, "no permalink key when none captured")
}
