// The metaagent OUT surfaces are dispatched off the NATS subject, which is the
// only session identity NATS authorizes for a publisher. This file pins two
// things the dedicated subscription cannot pin for itself: that the parse — not
// the subscription string — is what refuses a wrong-direction or nameless
// subject, and that a body which CLAIMS a session (an envelope-shaped publish)
// is cross-checked against the subject rather than trusted.
package main

import (
	"context"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

// sessionGetRecorder records every AgentSession key the handler looked up.
// The Get is the first observable act of an ACCEPTED message: everything
// before it is parsing, everything after needs a channel kind. Recording it
// makes "was this subject accepted, and for which session?" answerable without
// a kind that renders anything.
type sessionGetRecorder struct {
	client.Client
	mu   sync.Mutex
	keys []string
}

func (r *sessionGetRecorder) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, isSession := obj.(*spiceboxv1alpha1.AgentSession); isSession {
		r.mu.Lock()
		r.keys = append(r.keys, key.Namespace+"/"+key.Name)
		r.mu.Unlock()
	}
	return r.Client.Get(ctx, key, obj, opts...)
}

func (r *sessionGetRecorder) looked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

// metaagentAuthorityFixture builds two channel-bound sessions in one namespace
// so a body can plausibly claim the one its subject did not authorize.
func metaagentAuthorityFixture(t *testing.T) (*metaagentHandlers, *sessionGetRecorder) {
	t.Helper()
	scm := makeScheme()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c1"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "c1-creds"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c1-creds"},
		Data:       map[string][]byte{"k": []byte("v")},
	}
	session := func(name string) *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "fake"},
			},
		}
	}
	rec := &sessionGetRecorder{
		Client: fake.NewClientBuilder().WithScheme(scm).
			WithObjects(ch, sec, session("authorized"), session("victim")).Build(),
	}
	return &metaagentHandlers{cli: rec}, rec
}

// TestMetaagentDispatchSubjectIsTheAuthority covers the subject boundary and
// the envelope cross-check in one table, because both answer the same question:
// which session does this message act on?
func TestMetaagentDispatchSubjectIsTheAuthority(t *testing.T) {
	cases := []struct {
		name    string
		handler func(*metaagentHandlers) func(context.Context, *nats.Msg)
		subject string
		data    string
		want    []string // sessions the handler may look up; nil ⇒ none
	}{
		{
			name:    "out.metaagent_scope_approval, bare body: dispatched for the subject's session",
			handler: func(h *metaagentHandlers) func(context.Context, *nats.Msg) { return h.handleScopeApproval },
			subject: "ap.session.default.authorized.out.metaagent_scope_approval",
			data:    `{"requestId":"r1","requester":"U1"}`,
			want:    []string{"default/authorized"},
		},
		{
			name:    "in. segment on the OUT handler: refused, no session touched",
			handler: func(h *metaagentHandlers) func(context.Context, *nats.Msg) { return h.handleNotice },
			subject: "ap.session.default.authorized.in.metaagent_notice",
			data:    `{"requester":"U1","body":"hi"}`,
		},
		{
			name:    "notice subject on the scope-approval handler: refused, no session touched",
			handler: func(h *metaagentHandlers) func(context.Context, *nats.Msg) { return h.handleScopeApproval },
			subject: "ap.session.default.authorized.out.metaagent_notice",
			data:    `{"requester":"U1","body":"hi"}`,
		},
		{
			name:    "empty ns and name tokens: refused, no nameless lookup",
			handler: func(h *metaagentHandlers) func(context.Context, *nats.Msg) { return h.handleNotice },
			subject: "ap.session....",
			data:    `{"requester":"U1","body":"hi"}`,
		},
		{
			name:    "enveloped body claiming another session: dropped, victim never touched",
			handler: func(h *metaagentHandlers) func(context.Context, *nats.Msg) { return h.handleScopeApproval },
			subject: "ap.session.default.authorized.out.metaagent_scope_approval",
			data: `{"v":1,"kind":"metaagent_scope_approval",` +
				`"session":{"ns":"default","name":"victim"},` +
				`"publishedAt":"2026-08-12T00:00:00Z",` +
				`"payload":{"requestId":"r1","requester":"U1"}}`,
		},
		{
			name:    "enveloped body agreeing with the subject: dispatched for that session",
			handler: func(h *metaagentHandlers) func(context.Context, *nats.Msg) { return h.handleScopeApproval },
			subject: "ap.session.default.authorized.out.metaagent_scope_approval",
			data: `{"v":1,"kind":"metaagent_scope_approval",` +
				`"session":{"ns":"default","name":"authorized"},` +
				`"publishedAt":"2026-08-12T00:00:00Z",` +
				`"payload":{"requestId":"r1","requester":"U1"}}`,
			want: []string{"default/authorized"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rec := metaagentAuthorityFixture(t)
			tc.handler(h)(context.Background(), &nats.Msg{Subject: tc.subject, Data: []byte(tc.data)})
			assert.Equal(t, tc.want, nilIfEmpty(rec.looked()),
				"the session acted on must come from the subject, never from the body")
		})
	}
}

// nilIfEmpty normalizes an empty recording to nil so a table row can express
// "no session touched" as an absent want.
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
