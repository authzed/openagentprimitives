//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	eventnative "github.com/authzed/openagentprimitives/pkg/agent/sessionevents/native"
	channelregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	goalctrl "github.com/authzed/openagentprimitives/pkg/controllers/goals"
	goalsql "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	eventsql "github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	goalweb "github.com/authzed/openagentprimitives/pkg/web/goals"
	eventweb "github.com/authzed/openagentprimitives/pkg/web/sessionevents"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type goalRuntime struct {
	db         *sql.DB
	server     *goalweb.Server
	store      *sqlstore.Store
	events     *eventsql.Store
	dispatcher *goalctrl.Dispatcher
}

// startGoalsRuntime is opt-in: management-only scenarios keep their deliberate
// missing-execution assertion. This runtime uses production consent, routing,
// SQLite ledger, dispatcher, private receipts and the harness's real SpiceDB.
func (h *Harness) startGoalsRuntime(t *testing.T, mgr manager.Manager, signer *provenance.Signer) *goalRuntime {
	t.Helper()
	ctx := context.Background()
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "goal-runtime.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := goalsql.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	events := eventsql.New(db.DB(), false)
	require.NoError(t, events.Migrate(ctx))
	// GroupLinks mirrors internal/cmd/operator/main.go: without it every
	// group-owned domain fails validation and each group request is denied.
	service := &domain.Service{Store: store, GroupLinks: channelregistry.GroupMemberLinks}
	server := &goalweb.Server{Keys: h.tokensReg, Service: service, Reader: mgr.GetAPIReader(), Memory: h.memStore, Auth: h.SpiceDB}
	service.Auth, service.ExecutionAuth = server, server
	publish := h.nc.Publish
	consent := &goalweb.ConsentPublisher{Service: service, Memory: h.memStore, Writer: h.opSigned, Publish: publish}
	server.Consent = consent
	sources := sessionevents.NewRegistry()
	access := &eventweb.NativeSessions{Reader: mgr.GetAPIReader(), Memory: h.memStore, Auth: h.SpiceDB}
	sources.Register(&eventnative.Adapter{Keys: h.tokensReg, Authority: access, Access: access, Memory: h.memStore, Publishers: map[string]bool{"system:channelsd": true}})
	consumers := sessionevents.NewConsumers()
	consumers.Legacy = "goals"
	execution := &domain.EventExecution{Service: service, Sources: sources}
	triggers := &sessionevents.Triggers{Store: events, Observations: events, Authority: consumers}
	execution.Triggers = triggers
	service.Events = execution
	consumers.Register("goals", execution)
	server.EventSources = sources
	discovery := &goalweb.Discovery{Server: server, Store: store, Triggers: triggers, Sources: sources}
	server.Discovery = discovery
	consumers.Register("goal-discovery", discovery)
	dispatcher := &goalctrl.Dispatcher{Service: service, Store: store, Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Worker: "e2e-goals", DeliveryMemory: h.opSigned, RunGrants: server, EventRouter: &sessionevents.Router{Triggers: triggers, Store: events}, EventDispatcher: &sessionevents.Dispatcher{Triggers: triggers, Consumer: consumers}}
	server.ExecutionSessions = dispatcher
	require.NoError(t, mgr.Add(&goalweb.Publisher{Store: store, Memory: h.memStore, Signer: signer, Notify: consent.Notify}))
	require.NoError(t, mgr.Add(dispatcher))
	require.NoError(t, mgr.Add(discovery))
	h.runnerFactory.GoalServer = server
	h.runnerFactory.EventServer = &eventweb.Server{Ingester: &sessionevents.Ingester{Store: events, Adapters: sources}}
	// The prefs server assigns its shared token registry before mounting routes.
	require.True(t, h.runnerFactory.ensurePrefsServer())
	return &goalRuntime{db: db.DB(), server: server, store: store, events: events, dispatcher: dispatcher}
}

// GoalOccurrences returns every goal occurrence the dispatcher has started a
// session for, in due order. It is empty without Options.WithGoalExecution,
// the only mode that runs occurrences.
//
// It reads the occurrence ledger, not the AgentSessions: the operator removes
// a finished run's session, and the ledger is what records what each run
// reported, how it ended and what it delivered.
func (h *Harness) GoalOccurrences(ctx context.Context) ([]domain.Occurrence, error) {
	if h.goals == nil {
		return nil, nil
	}
	rows, err := h.goals.db.QueryContext(ctx, `SELECT id FROM oap_goal_occurrences WHERE session_uid <> '' ORDER BY due_at, id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]domain.Occurrence, 0, len(ids))
	for _, id := range ids {
		o, err := h.goals.store.Occurrence(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}
