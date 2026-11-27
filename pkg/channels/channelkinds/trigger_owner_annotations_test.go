package channelkinds_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// ownerOnlyReceiver names an owner but has no trust capability.
type ownerOnlyReceiver struct {
	channelkinds.WebhookReceiver
	subject string
	has     bool
	err     error
}

func (r ownerOnlyReceiver) TriggerOwnerSubject(*spiceboxv1alpha1.Channel, string, []byte) (string, bool, error) {
	return r.subject, r.has, r.err
}

// trustingReceiver names an owner and answers the trust question.
type trustingReceiver struct {
	ownerOnlyReceiver
	trusted  bool
	trustErr error
}

func (r trustingReceiver) TriggerOwnerTrusted(*spiceboxv1alpha1.Channel, string, []byte) (bool, error) {
	return r.trusted, r.trustErr
}

func TestTriggerOwnerAnnotations(t *testing.T) {
	const owner = "github_user:42#user"
	named := ownerOnlyReceiver{subject: owner, has: true}
	boom := errors.New("boom")

	cases := []struct {
		name    string
		recv    channelkinds.WebhookReceiver
		want    map[string]string
		wantErr bool
	}{
		{name: "no owner capability: nothing", recv: struct{ channelkinds.WebhookReceiver }{}},
		{name: "owner capability names nobody: nothing", recv: ownerOnlyReceiver{}},
		{name: "owner derivation error: nothing, error", recv: ownerOnlyReceiver{err: boom}, wantErr: true},
		{name: "owner without a trust capability: owner, untrusted", recv: named, want: map[string]string{
			spiceboxv1alpha1.AnnotationTriggerOwnerSubject: owner,
			spiceboxv1alpha1.AnnotationTriggerOwnerTrusted: spiceboxv1alpha1.AnnotationTriggerOwnerTrustedFalse,
		}},
		{name: "receiver vouches: owner, trusted", recv: trustingReceiver{ownerOnlyReceiver: named, trusted: true}, want: map[string]string{
			spiceboxv1alpha1.AnnotationTriggerOwnerSubject: owner,
			spiceboxv1alpha1.AnnotationTriggerOwnerTrusted: spiceboxv1alpha1.AnnotationTriggerOwnerTrustedTrue,
		}},
		{name: "receiver declines: owner, untrusted", recv: trustingReceiver{ownerOnlyReceiver: named}, want: map[string]string{
			spiceboxv1alpha1.AnnotationTriggerOwnerSubject: owner,
			spiceboxv1alpha1.AnnotationTriggerOwnerTrusted: spiceboxv1alpha1.AnnotationTriggerOwnerTrustedFalse,
		}},
		{name: "trust derivation error: owner, untrusted, error", recv: trustingReceiver{ownerOnlyReceiver: named, trusted: true, trustErr: boom}, want: map[string]string{
			spiceboxv1alpha1.AnnotationTriggerOwnerSubject: owner,
			spiceboxv1alpha1.AnnotationTriggerOwnerTrusted: spiceboxv1alpha1.AnnotationTriggerOwnerTrustedFalse,
		}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := channelkinds.TriggerOwnerAnnotations(tc.recv, &spiceboxv1alpha1.Channel{}, "pull_request", nil)
			if tc.wantErr {
				require.ErrorIs(t, err, boom)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
