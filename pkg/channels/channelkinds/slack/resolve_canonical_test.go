package slack

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestResolveSlackUserIDFromCanonical pins how channelsd turns a
// SpiceDB subject's canonical ObjectID (the base64-encoded form
// identity.Principal.Canonical() emits) into a real Slack user_id. This is the
// path the approval-DM flow was silently skipping in production: the
// resolved approver was `user:<base64(alice@example.com)>`, but
// the prior code only handled the literal `slack:<team>:<id>` form
// and dropped everything else with a log line, so no DM ever fired.
//
// Per the no-silent-errors mandate, every unresolvable case here is
// an explicit error — channelsd surfaces it instead of pretending
// the approval went out.
func TestResolveSlackUserIDFromCanonical(t *testing.T) {
	const (
		emailAlice = "alice@example.com"
		slackAlice = "UALICE001"
	)
	cases := []struct {
		name          string
		canonical     string
		lookupByEmail map[string]*slackapi.User
		lookupErr     error
		wantID        string
		wantErr       bool
		errContains   string
	}{
		{
			name:      "email canonical: Slack lookup by email succeeds",
			canonical: base64.RawURLEncoding.EncodeToString([]byte(emailAlice)),
			lookupByEmail: map[string]*slackapi.User{
				emailAlice: {ID: slackAlice, Profile: slackapi.UserProfile{Email: emailAlice}},
			},
			wantID: slackAlice,
		},
		{
			// Channelsd's pipeline stamps AnnotationStartedByCanonicalID +
			// envelope RecipientCanonical fields with the SpiceDB type-
			// prefixed subject form ("user:<base64>"). The resolver must
			// tolerate that and strip the prefix before base64-decoding.
			name:      "user:-prefixed canonical: prefix stripped, then resolved",
			canonical: "user:" + base64.RawURLEncoding.EncodeToString([]byte(emailAlice)),
			lookupByEmail: map[string]*slackapi.User{
				emailAlice: {ID: slackAlice, Profile: slackapi.UserProfile{Email: emailAlice}},
			},
			wantID: slackAlice,
		},
		{
			name:      "user:-prefixed slack-typed canonical: prefix stripped, then user_id extracted",
			canonical: "user:" + base64.RawURLEncoding.EncodeToString([]byte("slack:T123:"+slackAlice)),
			wantID:    slackAlice,
		},
		{
			name:          "email canonical: Slack user not found surfaces error (no silent skip)",
			canonical:     base64.RawURLEncoding.EncodeToString([]byte(emailAlice)),
			lookupByEmail: map[string]*slackapi.User{},
			wantErr:       true,
			errContains:   "slack",
		},
		{
			name:        "email canonical: Slack API errored",
			canonical:   base64.RawURLEncoding.EncodeToString([]byte(emailAlice)),
			lookupErr:   errors.New("rate_limited"),
			wantErr:     true,
			errContains: "rate_limited",
		},
		{
			name:      "slack-typed canonical (no email): extracts user_id",
			canonical: base64.RawURLEncoding.EncodeToString([]byte("slack::" + slackAlice)),
			wantID:    slackAlice,
		},
		{
			name:      "slack-typed canonical with team scope: extracts user_id",
			canonical: base64.RawURLEncoding.EncodeToString([]byte("slack:T123:" + slackAlice)),
			wantID:    slackAlice,
		},
		{
			name:        "garbled canonical (not valid base64): error",
			canonical:   "###not-base64###",
			wantErr:     true,
			errContains: "decode",
		},
		{
			name:        "empty canonical: error",
			canonical:   "",
			wantErr:     true,
			errContains: "empty",
		},
		{
			name:        "decoded form is neither email nor slack-typed: error",
			canonical:   base64.RawURLEncoding.EncodeToString([]byte("github::octocat")),
			wantErr:     true,
			errContains: "unrecognized",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeSlackClient{
				lookupByEmail:    tc.lookupByEmail,
				lookupByEmailErr: tc.lookupErr,
			}
			got, err := resolveSlackUserIDFromCanonical(context.Background(), fc, identity.CanonicalFromTrusted(tc.canonical, "test fixture"))
			if tc.wantErr {
				require.Error(t, err, "expected an error")
				if tc.errContains != "" {
					assert.Contains(t, err.Error(), tc.errContains, "error message")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, got)
		})
	}
}
