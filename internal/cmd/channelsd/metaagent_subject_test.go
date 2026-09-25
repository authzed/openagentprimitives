// What is left in channelsd for the metaagent flow once the rendering moved to
// the channel kind: reading the session and the kind back off the NATS subject.
// The Block Kit tests moved to pkg/channels/channelkinds/slack with the code they cover;
// the recipient-precedence tests moved to pkg/channels/channelkinds.
//
// There must be no local parser here: parsing must not lean on the subscription
// string happening to pin the in/out segment, which is a property of one line in
// main.go rather than of the handler. The rows below pin that boundary against
// the canonical parser, including an ignored segment and an empty ns/name.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestParseMetaagentOutSubject covers valid and invalid subject patterns for
// the two OUT subjects channelsd subscribes to.
func TestParseMetaagentOutSubject(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		wantNS  string
		wantN   string
		wantK   channelevents.Kind
		wantOK  bool
	}{
		{
			name:    "valid scope_approval: ns/name/kind extracted",
			subject: "ap.session.default.mysess.out.metaagent_scope_approval",
			wantNS:  "default",
			wantN:   "mysess",
			wantK:   channelevents.KindMetaagentScopeApproval,
			wantOK:  true,
		},
		{
			name:    "valid notice: ns/name/kind extracted",
			subject: "ap.session.kube-system.abc-123.out.metaagent_notice",
			wantNS:  "kube-system",
			wantN:   "abc-123",
			wantK:   channelevents.KindMetaagentNotice,
			wantOK:  true,
		},
		{
			name:    "in segment on an out parse: rejected",
			subject: "ap.session.default.mysess.in.metaagent_notice",
			wantOK:  false,
		},
		{
			name:    "empty ns and name tokens: rejected, not routed to a nameless session",
			subject: "ap.session....",
			wantOK:  false,
		},
		{
			name:    "too few parts: rejected",
			subject: "ap.session.ns",
			wantOK:  false,
		},
		{
			name:    "wrong prefix: rejected",
			subject: "not.session.ns.name.out.foo",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, k, ok := channelevents.ParseOutSubjectKind(tc.subject)
			assert.Equal(t, tc.wantOK, ok, "ok")
			if tc.wantOK {
				assert.Equal(t, tc.wantNS, ns, "ns")
				assert.Equal(t, tc.wantN, name, "name")
				assert.Equal(t, tc.wantK, k, "kind")
			}
		})
	}
}
