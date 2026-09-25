package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSubject(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		allowed []SubjectType
		ok      bool
	}{
		{"service allowed", "service:hubspot-digest-bot", []SubjectType{SubjectService}, true},
		{"user blocked when only service allowed", "user:dmljdGltQGNvcnAuY29t", []SubjectType{SubjectService}, false},
		{"group blocked when only service allowed", "group:admins", []SubjectType{SubjectService}, false},
		{"user allowed when user permitted", "user:dmljdGltQGNvcnAuY29t", []SubjectType{SubjectUser}, true},
		{"agentsession id with slash", "agentsession:ns/sess-1", []SubjectType{SubjectAgentSession}, true},
		{"relation suffix rejected for concrete subject", "group:admins#member", []SubjectType{SubjectGroup}, false},
		{"missing type prefix", "hubspot-digest-bot", []SubjectType{SubjectService}, false},
		{"empty id after prefix", "service:", []SubjectType{SubjectService}, false},
		{"empty string", "", []SubjectType{SubjectService}, false},
		{"no allowed types rejects all", "service:foo", nil, false},
		{"multiple allowed types", "service:foo", []SubjectType{SubjectUser, SubjectService}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSubject(tc.subject, tc.allowed...)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestValidateSubjectSet(t *testing.T) {
	// Subject-set form: the optional #relation IS permitted.
	require.NoError(t, ValidateSubjectSet("group:eng#member", SubjectGroup))
	require.NoError(t, ValidateSubjectSet("group:eng", SubjectGroup), "relation is optional")
	// Type allowlist still enforced.
	assert.Error(t, ValidateSubjectSet("user:abc#member", SubjectGroup))
	// Format still enforced.
	assert.Error(t, ValidateSubjectSet("group:eng#", SubjectGroup), "empty relation is malformed")
	assert.Error(t, ValidateSubjectSet("nocolon", SubjectGroup))
}

// TestParseAgentSessionSubject covers the one parser three packages now share
// for "agentsession:<ns>/<name>" — Channel.spec.authzSubject, an agent
// message's From, and the acting subject of an inbound.
//
// The charset rows are why it was consolidated: the copy in the agent kind's
// Sender checked only the prefix and the '/', so an id could carry anything at
// all and was kept clean solely by the CRD Pattern in channel_types.go
// happening to agree with subjectRE. The ':' row matters most — SpiceDB
// rejects such an object id as InvalidArgument, which surfaces as an
// infrastructure fault rather than as a refusal.
func TestParseAgentSessionSubject(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    SessionRef // the zero value means "must be refused"
	}{
		{
			name:    "well-formed reference: parsed into its two halves",
			subject: "agentsession:demo-ns/lead-1",
			want:    SessionRef{Namespace: "demo-ns", Name: "lead-1"},
		},
		{
			name:    "dotted namespace and name: k8s-legal, and inside the charset",
			subject: "agentsession:demo.ns/lead.1",
			want:    SessionRef{Namespace: "demo.ns", Name: "lead.1"},
		},

		{name: "user subject: refused, never cut into a namespace and name", subject: "user:YWxpY2U"},
		{name: "service subject: refused, a cron identity has no session end", subject: "service:nightly"},
		{name: "unregistered type: refused", subject: "robot:hal"},
		{name: "no type prefix at all: refused", subject: "demo-ns/lead-1"},
		{name: "empty subject: refused", subject: ""},
		{name: "no namespace separator: refused", subject: "agentsession:lead-1"},
		{name: "empty namespace: refused", subject: "agentsession:/lead-1"},
		{name: "empty name: refused", subject: "agentsession:demo-ns/"},
		{name: "relation suffix: refused, that names a set and not an end", subject: "agentsession:demo-ns/lead-1#parent"},
		{name: "colon in the id: refused, SpiceDB rejects such an object id outright", subject: "agentsession:demo-ns/lead:1"},
		{name: "space in the id: refused", subject: "agentsession:demo-ns/lead 1"},
		{name: "newline in the id: refused, log-injection shaped", subject: "agentsession:demo-ns/lead\n1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentSessionSubject(tc.subject)
			if tc.want == (SessionRef{}) {
				require.Error(t, err)
				assert.Equal(t, SessionRef{}, got, "a refused subject must yield no reference to act on")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
