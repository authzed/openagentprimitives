package capability

import (
	"context"
	"testing"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func TestGoalsCapabilityRequiresSafeEnvironment(t *testing.T) {
	c := goalsCapability{}
	assert.False(t, c.DefaultOn())
	o := OfferContext{Class: &v1.AgentClass{}, Session: &v1.AgentSession{}}
	o.Class.Spec.Authz = &v1.AuthzBlock{InformationLeakage: &v1.InformationLeakagePolicy{Mode: "enforcing"}}
	o.Env.MemoryAvailable = true
	o.Env.GoalsCaller = func(context.Context, core.Request) (core.Response, error) { return core.Response{}, nil }
	tools, skip := c.Offer(o)
	assert.Nil(t, skip)
	names := []string{}
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	assert.ElementsMatch(t, []string{"list_goals", "get_goal", "list_goal_runs", "create_goal", "update_goal", "request_goal_execution", "request_goal_discovery", "get_goal_discovery_policy", "get_goal_discovery_proposal", "stop_goal_discovery"}, names)
	o.Class.Spec.Authz = &v1.AuthzBlock{InformationLeakage: &v1.InformationLeakagePolicy{Mode: "logging"}}
	tools, skip = c.Offer(o)
	assert.NotNil(t, skip)
	assert.Empty(t, tools)
	o.Class.Spec.Authz.InformationLeakage.Mode = "enforcing"
	o.Env.GoalsCaller = nil
	tools, skip = c.Offer(o)
	assert.NotNil(t, skip)
	assert.Empty(t, tools)
}
