//go:build integration

package webhookqueue

import (
	"context"
	"os"
	"testing"
	"time"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/stretchr/testify/require"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }

func TestAdmissionEvidenceImmutableWhileLeaseStatusUpdates(t *testing.T) {
	te := testenv.Shared(t)
	ctx := context.Background()
	record := &api.WebhookAdmission{ObjectMeta: meta.ObjectMeta{Name: "synthetic-admission", Namespace: "default"}, Spec: api.WebhookAdmissionSpec{RecordType: "delivery", ChannelName: "synthetic-ci", ChannelUID: "synthetic-uid", Event: "workflow_run", Body: []byte(`{"run":42}`), EnqueuedAt: meta.Now()}}
	require.NoError(t, te.Client.Create(ctx, record), "the generated queue CRD and its CEL rule must install and accept valid evidence")
	t.Cleanup(func() { require.NoError(t, te.Client.Delete(context.Background(), record)) })
	key := client.ObjectKeyFromObject(record)
	record.Spec.Body = []byte(`{"run":99}`)
	err := te.Client.Update(ctx, record)
	require.True(t, kerrors.IsInvalid(err), "immutable provider body must reject rewriting: %v", err)
	require.ErrorContains(t, err, "webhook admission evidence is immutable")
	require.NoError(t, te.Client.Get(ctx, key, record))
	require.JSONEq(t, `{"run":42}`, string(record.Spec.Body))
	record.Status.Phase = "pending"
	record.Status.LeaseUntil = meta.NewTime(time.Now().Add(time.Minute))
	record.Status.RetryCount = 1
	require.NoError(t, te.Client.Status().Update(ctx, record), "worker lease updates must not rewrite evidence")
	require.NoError(t, te.Client.Get(ctx, key, record))
	require.EqualValues(t, 1, record.Status.RetryCount)
	require.JSONEq(t, `{"run":42}`, string(record.Spec.Body))
}

// Exercise padding and the standard base64 alphabet. Kubernetes CEL byte
// conversion rejects + and / on status writes despite accepting creation.
func TestAdmissionLeasePreservesStandardBase64Payload(t *testing.T) {
	te := testenv.Shared(t)
	for _, tc := range []struct {
		name string
		body []byte
	}{{"one", []byte("x")}, {"two", []byte("xx")}, {"three", []byte("xxx")}, {"slash", []byte{255}}, {"plus", []byte{251}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			body := tc.body
			record := &api.WebhookAdmission{ObjectMeta: meta.ObjectMeta{GenerateName: "payload-padding-", Namespace: "default"}, Spec: api.WebhookAdmissionSpec{RecordType: "delivery", ChannelName: "synthetic-ci", ChannelUID: "synthetic-uid", Event: "workflow_run", Body: body, EnqueuedAt: meta.Now()}}
			require.NoError(t, te.Client.Create(ctx, record))
			t.Cleanup(func() { require.NoError(t, te.Client.Delete(context.Background(), record)) })
			record.Status.Phase = "pending"
			require.NoError(t, te.Client.Status().Update(ctx, record))
			require.NoError(t, te.Client.Get(ctx, client.ObjectKeyFromObject(record), record))
			require.Equal(t, body, record.Spec.Body)
			record.Spec.Body = []byte("changed")
			err := te.Client.Update(ctx, record)
			require.True(t, kerrors.IsInvalid(err))
			require.ErrorContains(t, err, "webhook admission evidence is immutable")
		})
	}
}
