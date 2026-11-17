package goals

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// decisionIngressHTTP tells channelsd whether a decision on a goal run may
// arrive on the run's ordinary inbound subject: true only for the live run of
// a group-owned goal. The answer is the occurrence's domain, read only after
// ValidateGoalSession has bound the session to that occurrence (UID, name,
// reference, still running) — the same gate the run's own authorize_execution
// passes — so nothing on the session a caller could write can make a private
// run, a concluded run or an impostor read as a group run. Who may decide is
// not answered here; channelsd's decider policy still checks the clicker.
func (s *Server) decisionIngressHTTP(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || s.Tokens == nil || !s.Tokens.IsChannelsdToken(bearer) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if s.Reader == nil || s.Service == nil || s.ExecutionSessions == nil {
		http.Error(w, "goal service unavailable", 503)
		return
	}
	var check domain.RunDecisionIngressCheck
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&check); err != nil || check.Namespace == "" || check.Name == "" {
		http.Error(w, "invalid decision ingress check", 400)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid decision ingress check", 400)
		return
	}
	ctx := memory.WithCaller(memory.WithSystemApproval(r.Context(), "system:operator"), "system:operator")
	var sess v1.AgentSession
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: check.Namespace, Name: check.Name}, &sess); err != nil {
		s.fail(w, r, err)
		return
	}
	if sess.Spec.GoalExecution == nil {
		s.fail(w, r, domain.ErrDenied)
		return
	}
	if err := s.ExecutionSessions.ValidateGoalSession(ctx, &sess); err != nil {
		s.fail(w, r, err)
		return
	}
	group, err := s.groupRun(ctx, &sess)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(domain.RunDecisionIngress{Group: group}); err != nil {
		s.fail(w, r, err)
	}
}
