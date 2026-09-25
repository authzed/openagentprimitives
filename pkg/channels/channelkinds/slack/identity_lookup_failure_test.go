package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestResolveIdentity_LookupFailureFallsBackToInstalledTeam is the regression
// guard for a phantom-subject defect: when users.info fails (a rate limit, a
// transient 5xx), resolveIdentity returned an identity with an empty Email AND
// an empty TeamScope. Principal.Canonical has no empty-teamScope guard, so the
// synthetic subject became base64("slack::U…") — a canonical that matches
// neither the user's email subject nor the slack:<team>:<user> fallback the
// function's own doc promises. The affected user is durably mis-attributed:
// denied interact on their own session, and left behind an orphan
// cluster-scoped UserIdentity with an empty domain.
//
// The error was also never logged, returned, or surfaced, so the blip left no
// trace at all — the no-silent-errors rule's third strike.
func TestResolveIdentity_LookupFailureFallsBackToInstalledTeam(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"ratelimited"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	l := &slackListener{
		api:             slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/")),
		idents:          NewIdentityCache(4),
		installedTeamID: "T0COMPANY",
	}

	id := l.resolveIdentity(log.IntoContext(context.Background(), capLogger), "U0ALICE")

	assert.Equal(t, identity.Email(""), id.Email,
		"a failed lookup must not invent an email")
	assert.Equal(t, identity.TeamScope("T0COMPANY"), id.TeamScope,
		"a failed lookup must fall back to the installed team so the synthetic "+
			"canonical is slack:<team>:<user>, not slack::<user>")

	canon, err := identity.FromExternal(id.Kind, id.TeamScope, id.ExternalID, id.Email).
		AllowSynthetic().Canonical()
	require.NoError(t, err, "synthetic canonical must derive")
	want, err := identity.FromExternal(identity.Kind(KindName), "T0COMPANY", "U0ALICE", "").
		AllowSynthetic().Canonical()
	require.NoError(t, err)
	assert.Equal(t, want, canon,
		"the canonical minted on the error path must be the documented team-scoped one")

	_, cached := l.idents.Get("U0ALICE")
	assert.False(t, cached,
		"a failed lookup must not be cached: the next event should retry, not "+
			"pin the fallback for the life of the process")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "the users.info failure must be logged, not silently dropped")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "U0ALICE", "log must carry the Slack user id")
	assert.Contains(t, joined, "ratelimited", "log must carry the underlying error")
}
