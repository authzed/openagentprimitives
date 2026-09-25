package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubject(t *testing.T) {
	assert.Equal(t, "user:YWxpY2U", Subject("user:YWxpY2U").String())
	assert.True(t, Subject("").Empty())
	assert.False(t, Subject("user:YWxpY2U").Empty())
}

// TestCanonicalUserID_Subject is a golden-covered no-op: the conversion must
// produce exactly "user:" + the bare canonical bytes — the same bytes every
// hand-built `"user:"+canon.String()` wire-DTO producer used to construct.
func TestCanonicalUserID_Subject(t *testing.T) {
	assert.Equal(t, Subject("user:YWxpY2U"), canonicalUnverified("YWxpY2U", "test fixture").Subject())
	assert.Equal(t, Subject("user:"), canonicalUnverified("", "test fixture").Subject())
}

// TestCanonicalUserID_SubjectRef covers the conversion callers reach for when
// the canonical is whatever principal happened to act — which is not always a
// human. The no-human inbound path assigns the Channel's authzSubject verbatim,
// so a CanonicalUserID can already carry a type prefix, and prefixing it a
// second time yields "user:service:…" — a subject that matches nothing.
func TestCanonicalUserID_SubjectRef(t *testing.T) {
	cases := []struct {
		name      string
		canonical CanonicalUserID
		want      Subject
	}{
		{
			name:      "bare canonical: prefixed as a user, same bytes as Subject()",
			canonical: canonicalUnverified("YWxpY2U", "test fixture"),
			want:      "user:YWxpY2U",
		},
		{
			name:      "already-qualified service subject: preserved, not double-prefixed",
			canonical: canonicalUnverified("service:nightly-report", "test fixture"),
			want:      "service:nightly-report",
		},
		{
			name:      "already-qualified user subject: preserved, not double-prefixed",
			canonical: canonicalUnverified("user:YWxpY2U", "test fixture"),
			want:      "user:YWxpY2U",
		},
		{
			name:      "empty: no principal to name",
			canonical: canonicalUnverified("", "test fixture"),
			want:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.canonical.SubjectRef())
		})
	}
}

func TestSubject_ObjectType(t *testing.T) {
	cases := []struct {
		name    string
		subject Subject
		want    string
	}{
		{"user subject", "user:ABC", "user"},
		{"group subject", "group:x", "group"},
		{"empty subject", "", ""},
		{"no colon", "noColon", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.subject.ObjectType())
		})
	}
}

func TestSubject_CanonicalUserID(t *testing.T) {
	cases := []struct {
		name    string
		subject Subject
		want    CanonicalUserID
		wantErr error
	}{
		{"user subject", "user:ABC", canonicalUnverified("ABC", "test fixture"), nil},
		{"user subject empty body", "user:", canonicalUnverified("", "test fixture"), nil},
		{"group subject fails closed", "group:eng#member", canonicalUnverified("", "test fixture"), ErrNonUserSubject},
		{"serviceaccount subject fails closed", "serviceaccount:sa1", canonicalUnverified("", "test fixture"), ErrNonUserSubject},
		{"empty subject fails closed", "", canonicalUnverified("", "test fixture"), ErrNonUserSubject},
		{"no colon fails closed", "noColon", canonicalUnverified("", "test fixture"), ErrNonUserSubject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.subject.CanonicalUserID()
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSubject_CanonicalUserID_RoundTrip proves CanonicalUserID.Subject() and
// Subject.CanonicalUserID() are inverses for any bare canonical.
func TestSubject_CanonicalUserID_RoundTrip(t *testing.T) {
	for _, c := range []CanonicalUserID{canonicalUnverified("YWxpY2U", "test fixture"), canonicalUnverified("", "test fixture"), canonicalUnverified("c2xhY2s6VDEyMzpVNDU2", "test fixture")} {
		got, err := c.Subject().CanonicalUserID()
		require.NoError(t, err)
		assert.Equal(t, c, got)
	}
}
