package goals

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// refusingRuns is a dispatcher that no longer recognises the run (concluded,
// impostor, or otherwise unbound).
type refusingRuns struct{}

func (refusingRuns) ValidateGoalSession(context.Context, *v1.AgentSession) error {
	return core.ErrDenied
}

// decisionIngress asks the server, under token, whether a decision on ns/name
// may arrive by channel ingress.
func decisionIngress(t *testing.T, s *Server, token, ns, name string) (int, core.RunDecisionIngress) {
	t.Helper()
	raw, err := json.Marshal(core.RunDecisionIngressCheck{Namespace: ns, Name: name})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/goals/decision/ingress", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out core.RunDecisionIngress
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	}
	return w.Code, out
}

func TestDecisionIngressGroupRun(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		session string
		refuse  bool
		status  int
		group   bool
	}{
		{name: "channelsd asks about a live group run: 200, group", token: "channelsd", session: "run", status: 200, group: true},
		{name: "the run's own token: 401", token: "run-token", session: "run", status: 401},
		{name: "a run the dispatcher no longer binds: refused 404", token: "channelsd", session: "run", refuse: true, status: 404},
		{name: "no such session: refused (lookup error, 500)", token: "channelsd", session: "absent", status: 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := groupRunServer(t)
			r.c.f.s.Tokens.SetChannelsdToken("channelsd")
			if tc.refuse {
				r.c.f.s.ExecutionSessions = refusingRuns{}
			}
			status, out := decisionIngress(t, r.c.f.s, tc.token, groupNS, tc.session)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.group, out.Group)
		})
	}
}
