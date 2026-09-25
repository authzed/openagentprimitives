package hooks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingReceived captures the MetaagentReceived hook's SetKind side effect
// and scripts the manage_scope checker.
type recordingReceived struct {
	allowed     bool
	checkErr    error
	checkCalls  int
	kind        string
	kindWritten bool
}

func (r *recordingReceived) deps(wireChecker bool) hooks.MetaagentReceivedDeps {
	d := hooks.MetaagentReceivedDeps{
		SetKind: func(kind string) {
			r.kind = kind
			r.kindWritten = true
		},
	}
	if wireChecker {
		d.CheckManageScope = func(_ context.Context, _, _ string, _ identity.CanonicalUserID) (bool, error) {
			r.checkCalls++
			return r.allowed, r.checkErr
		}
	}
	return d
}

func receivedInput(kind, requester string) pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.MetaagentReceived,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
		Requester: identity.CanonicalFromTrusted(requester, "test fixture"),
		Metaagent: &pipeline.MetaagentInfo{Kind: kind, Requester: requester},
	}
}

// TestMetaagentReceived_ColdStart_SkipsGate_Allows verifies cold_start is
// tautologically allowed (its requester is definitionally started_by) and the
// manage_scope SpiceDB check is NOT consulted.
func TestMetaagentReceived_ColdStart_SkipsGate_Allows(t *testing.T) {
	rec := &recordingReceived{allowed: false} // would deny if consulted
	h := hooks.NewMetaagentReceived(rec.deps(true /*wireChecker*/))
	dec := h.Eval(context.Background(), receivedInput("cold_start", "user:alice"))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "cold_start skips the manage_scope gate")
	assert.Equal(t, 0, rec.checkCalls, "cold_start must NOT consult the manage_scope checker")
	assert.True(t, rec.kindWritten)
	assert.Equal(t, "cold_start", rec.kind)
}

// TestMetaagentReceived_MidSession_Owner_Allows verifies the owner passes the
// manage_scope gate and the kind is stamped.
func TestMetaagentReceived_MidSession_Owner_Allows(t *testing.T) {
	rec := &recordingReceived{allowed: true}
	h := hooks.NewMetaagentReceived(rec.deps(true))
	dec := h.Eval(context.Background(), receivedInput("mid_session", "user:alice"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Equal(t, 1, rec.checkCalls, "mid_session must consult the manage_scope checker")
	assert.Equal(t, "mid_session", rec.kind)
}

// TestMetaagentReceived_MidSession_NonOwner_Denies verifies a non-owner is
// denied (the intended tightening: a thread participant can #interact but not
// change scope).
func TestMetaagentReceived_MidSession_NonOwner_Denies(t *testing.T) {
	rec := &recordingReceived{allowed: false}
	h := hooks.NewMetaagentReceived(rec.deps(true))
	dec := h.Eval(context.Background(), receivedInput("mid_session", "user:bob"))
	assert.Equal(t, pipeline.Deny, dec.Verdict, "non-owner denied manage_scope")
	assert.Equal(t, 1, rec.checkCalls)
}

// TestMetaagentReceived_MidSession_CheckError_Denies verifies a check error
// fails closed (Deny).
func TestMetaagentReceived_MidSession_CheckError_Denies(t *testing.T) {
	rec := &recordingReceived{checkErr: errors.New("spicedb down")}
	h := hooks.NewMetaagentReceived(rec.deps(true))
	dec := h.Eval(context.Background(), receivedInput("mid_session", "user:alice"))
	assert.Equal(t, pipeline.Deny, dec.Verdict, "check error fails closed")
}

// TestMetaagentReceived_MidSession_UnwiredChecker_Denies verifies a nil checker
// fails closed (Deny). In a running authzd this never occurs (SpiceDB is
// required at startup), but the defensive path Denies.
func TestMetaagentReceived_MidSession_UnwiredChecker_Denies(t *testing.T) {
	rec := &recordingReceived{}
	h := hooks.NewMetaagentReceived(rec.deps(false /*no checker*/))
	dec := h.Eval(context.Background(), receivedInput("mid_session", "user:alice"))
	assert.Equal(t, pipeline.Deny, dec.Verdict, "unwired checker fails closed")
	// Kind is still stamped (classification is independent of the gate).
	assert.Equal(t, "mid_session", rec.kind)
}

func TestMetaagentReceived_Points(t *testing.T) {
	h := hooks.NewMetaagentReceived(hooks.MetaagentReceivedDeps{})
	assert.Equal(t, []pipeline.Point{pipeline.MetaagentReceived}, h.Points())
	assert.Equal(t, "metaagent_received", h.Name())
}

var _ pipeline.Hook = hooks.NewMetaagentReceived(hooks.MetaagentReceivedDeps{})
