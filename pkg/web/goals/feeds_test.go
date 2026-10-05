package goals

import (
	"context"
	"encoding/json"
	"testing"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	native "github.com/authzed/openagentprimitives/pkg/agent/sessionevents/native"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	eventweb "github.com/authzed/openagentprimitives/pkg/web/sessionevents"
	"github.com/stretchr/testify/require"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestConfiguredFeedsRequireCurrentSourceAccess(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	var class v1.AgentClass
	require.NoError(t, f.s.Reader.Get(ctx, client.ObjectKey{Namespace: "team", Name: "assistant"}, &class))
	class.Spec.Capabilities["goals"] = apiext.JSON{Raw: []byte(`{"eventFeeds":[{"name":"Flight updates","description":"Configured flight feed","source":{"kind":"native","namespace":"team","id":"team/session"},"eventKinds":["flight.changed"]}]}`)}
	require.NoError(t, f.s.Reader.(client.Client).Update(ctx, &class))
	access := &eventweb.NativeSessions{Reader: f.s.Reader, Memory: f.mem, Auth: f.auth}
	registry := sessionevents.NewRegistry()
	registry.Register(&native.Adapter{Access: access})
	f.s.EventSources = registry
	a := domain.Actor{Session: "team/session", Domain: domain.Domain{Namespace: "team", Class: "assistant", ClassUID: "class-uid", Owner: "alice"}}
	feeds, err := f.s.eventFeedsFor(ctx, a)
	require.NoError(t, err)
	require.Len(t, feeds, 1)
	require.Equal(t, "session-uid", feeds[0].Source.UID)
	require.Equal(t, []string{"flight.changed"}, feeds[0].EventKinds)
	dependencies := (domain.Response{EventFeeds: feeds}).ReadDependencies()
	require.Contains(t, dependencies, domain.Source{ResourceType: "agentsession", ResourceID: "team/session", Permission: "read_transcript"})
	f.auth.denySource = true
	feeds, err = f.s.eventFeedsFor(ctx, a)
	require.NoError(t, err)
	require.Empty(t, feeds, "revocation must hide source metadata")
	f.auth.denySource = false
	var sess v1.AgentSession
	require.NoError(t, f.s.Reader.Get(ctx, client.ObjectKey{Namespace: "team", Name: "session"}, &sess))
	require.NoError(t, f.s.Reader.(client.Client).Delete(ctx, &sess))
	feeds, err = f.s.eventFeedsFor(ctx, a)
	require.NoError(t, err)
	require.Empty(t, feeds, "deleted sources cannot be advertised")
	// Bad configuration must surface, never silently masquerade as a valid feed.
	class.Spec.Capabilities["goals"] = apiext.JSON{Raw: []byte(`{"eventFeeds":[{"name":"broken"}]}`)}
	require.NoError(t, f.s.Reader.(client.Client).Update(ctx, &class))
	_, err = f.s.eventFeedsFor(ctx, a)
	require.ErrorIs(t, err, domain.ErrInvalid)
	_, err = domain.ParseConfig(json.RawMessage(`{"eventFeeds":[{"name":"feed","source":{"kind":"native","namespace":"team","id":"team/session"},"eventKinds":["changed"],"sources":[{"resourceType":"document"}]}]}`))
	require.ErrorIs(t, err, domain.ErrInvalid, "class authors cannot invent source dependencies")
	class.Spec.Capabilities["goals"] = apiext.JSON{Raw: []byte(`{"enabled":false}`)}
	require.NoError(t, f.s.Reader.(client.Client).Update(ctx, &class))
	_, err = f.s.eventFeedsFor(ctx, a)
	require.ErrorIs(t, err, domain.ErrDenied, "disabled goals cannot disclose feed metadata")
}
