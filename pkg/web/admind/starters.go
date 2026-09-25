package admind

import (
	"context"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// sessionMetaRow carries the per-session facts the admin rollups join onto
// audit rows: the creating user's canonical subject and the session's start
// time. No controller stamps status.startedAt today, so StartedAt falls back to
// the CR's creation time (the same fallback the live aggregator applies).
type sessionMetaRow struct {
	StartedBy string
	StartedAt *time.Time
}

// sessionMeta lists the AgentSessions and indexes each one's start metadata
// (startedBy + startedAt) by "ns/name". Every session is present (so startedAt
// is available even for kubectl-driven sessions that carry no started-by
// annotation); StartedBy is empty for those. A non-nil map (even empty) means
// "data available"; a returned error means the List failed and the caller
// should degrade rather than fabricate.
func (a *Admind) sessionMeta(ctx context.Context) (map[string]sessionMetaRow, error) {
	var list spiceboxv1alpha1.AgentSessionList
	if err := a.cfg.K8s.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make(map[string]sessionMetaRow, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		row := sessionMetaRow{StartedBy: spiceboxv1alpha1.StartedBySubject(s).String()}
		if s.Status.StartedAt != nil {
			t := s.Status.StartedAt.Time
			row.StartedAt = &t
		} else if !s.CreationTimestamp.IsZero() {
			t := s.CreationTimestamp.Time
			row.StartedAt = &t
		}
		out[s.Namespace+"/"+s.Name] = row
	}
	return out, nil
}

// sessionStarters indexes each session's creating-user canonical subject (the
// started-by-canonical-id annotation channelsd stamps at session creation) by
// "ns/name". kubectl-driven sessions carry no annotation and are simply absent
// from the map — callers key those under their own unknown/blank fallback. A
// non-nil map (even empty) means "starter data available"; a returned error
// means the List failed and the caller should degrade rather than fabricate.
//
// Shared by the budget breakdown (byUser axis) and the audit sessions rollup —
// both best-effort: a List error → the caller logs and proceeds with an
// empty/nil map, never failing the request. Delegates to sessionMeta so the
// AgentSession List + index lives in one place.
func (a *Admind) sessionStarters(ctx context.Context) (map[string]string, error) {
	meta, err := a.sessionMeta(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(meta))
	for k, m := range meta {
		if m.StartedBy != "" {
			out[k] = m.StartedBy
		}
	}
	return out, nil
}
