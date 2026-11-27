package httpsrv_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferenceaccess"
)

// TestPreferencesGetUserRef_NameRefAuditIsNotSessionReadable pins that the
// agent cannot read back what the response withheld. A name-resolved read
// blanks the subject in its answer, but the preference_access entry it appends
// to the session's own scope records that subject (the matched person's
// encoded email). The session credential must be refused that Kind by name,
// and must not receive it from a sweep that names no Kind.
func TestPreferencesGetUserRef_NameRefAuditIsNotSessionReadable(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, trustedTriggerSession(),
		userIdentityFor(t, "alice@example.com", "amoreno", "Alice Moreno"))

	status, body := getUserRefRaw(t, srv.URL, "name:Alice Moreno")
	require.Equal(t, http.StatusOK, status, body)
	audited := listAudit(t, mem)
	require.Len(t, audited, 1, "the platform still holds the record")
	require.Equal(t, mustCanonicalEmail(t, "alice@example.com"), audited[0].ResolvedSubject)

	scope := memory.Scope{Kind: "session", ID: prefsNS + "/" + prefsSession}
	asSession := httpclient.New(srv.URL, prefsToken)

	_, err := asSession.Query(context.Background(), memory.Query{Scope: scope, Kinds: []string{preferenceaccess.KindName}})
	assert.ErrorIs(t, err, memory.ErrKindNotSessionReadable,
		"a session credential naming preference_access must be refused, not answered")

	sweep, err := asSession.Query(context.Background(), memory.Query{Scope: scope})
	require.NoError(t, err, "a sweep naming no Kind still succeeds")
	for _, e := range sweep.Entries {
		assert.NotEqual(t, preferenceaccess.KindName, e.Kind, "the sweep must not carry the audit entry")
	}
}
