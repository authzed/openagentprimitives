package browserstart

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
)

// fakeDeps is a minimal Deps for exercising StartableIn directly (never
// through Start, which needs the full reserve/create/adopt sequence — see
// pkg/web/webui/sessions and pkg/web/webui/agentui for those fixtures). Only
// the fields StartableIn actually reads are configurable; the rest exist
// solely to satisfy the interface.
type fakeDeps struct {
	startable []string
	// workshops maps a subject to the workshop namespaces WorkshopNamespacesFor
	// should answer for it — a stand-in for the Workshop controller's per-owner
	// RBAC binding this method reads in production.
	workshops    map[string][]string
	workshopsErr error

	// workshopCalls counts WorkshopNamespacesFor invocations — how
	// TestStartableIn_StaticHit_SkipsTheWorkshopLookup proves the round trip
	// is skipped entirely rather than merely ignored.
	workshopCalls int
}

func (d *fakeDeps) StartBrowserSession() browsersession.StartFunc { return nil }
func (d *fakeDeps) LiveSessions() LiveSessions                    { return nil }
func (d *fakeDeps) StartableNamespaces() []string                 { return d.startable }
func (d *fakeDeps) Logger() logr.Logger                           { return logr.Discard() }
func (d *fakeDeps) WorkshopNamespacesFor(_ context.Context, subject string) ([]string, error) {
	d.workshopCalls++
	if d.workshopsErr != nil {
		return nil, d.workshopsErr
	}
	return d.workshops[subject], nil
}

// TestStartableIn_UnionsTheViewersWorkshops proves the dynamic arm ADDS to the
// static one rather than replacing it, and that it is scoped per-subject: a
// workshop belongs to the viewer who owns it, never to every viewer.
func TestStartableIn_UnionsTheViewersWorkshops(t *testing.T) {
	d := &fakeDeps{startable: []string{"default"}, workshops: map[string][]string{"user:a": {"ws-1"}}}
	assert.True(t, StartableIn(context.Background(), d, "user:a", "default"))
	assert.True(t, StartableIn(context.Background(), d, "user:a", "ws-1"), "a workshop the viewer owns is startable")
	assert.False(t, StartableIn(context.Background(), d, "user:b", "ws-1"), "another viewer's workshop is not")
	assert.False(t, StartableIn(context.Background(), d, "user:a", "ws-2"))
}

// TestStartableIn_WorkshopLookupErrorIsLoggedNotStartable proves the dynamic
// arm fails CLOSED on a lookup error without taking the static arm down with
// it — a transient failure resolving the viewer's workshops must not also
// disable the configured --session-start-namespaces set.
func TestStartableIn_WorkshopLookupErrorIsLoggedNotStartable(t *testing.T) {
	d := &fakeDeps{startable: []string{"default"}, workshopsErr: errors.New("boom")}
	assert.True(t, StartableIn(context.Background(), d, "user:a", "default"), "the static arm is unaffected")
	assert.False(t, StartableIn(context.Background(), d, "user:a", "ws-1"), "fail closed on the dynamic arm")
}

// TestStartableIn_StaticHit_SkipsTheWorkshopLookup proves the static-set
// short-circuit actually skips WorkshopNamespacesFor rather than merely
// tolerating its answer — the property that keeps Start's common-case door
// (a static namespace like "default") from paying for a Workshop List it
// does not need.
func TestStartableIn_StaticHit_SkipsTheWorkshopLookup(t *testing.T) {
	d := &fakeDeps{startable: []string{"default"}}
	assert.True(t, StartableIn(context.Background(), d, "user:a", "default"))
	assert.Equal(t, 0, d.workshopCalls, "a static match must never reach the workshop lookup")
}

// TestStartableInSet proves the pre-resolved-set variant sessions' picker
// uses is exactly the same union StartableIn checks, over both arms and
// their absence.
func TestStartableInSet(t *testing.T) {
	static := []string{"default"}
	owned := []string{"ws-1"}
	assert.True(t, StartableInSet(static, owned, "default"), "the static arm")
	assert.True(t, StartableInSet(static, owned, "ws-1"), "the dynamic arm")
	assert.False(t, StartableInSet(static, owned, "ws-2"), "neither arm names it")
	assert.False(t, StartableInSet(static, nil, "ws-1"), "an empty dynamic set contributes nothing")
}
