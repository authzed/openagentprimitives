package capability

import (
	"context"
	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"testing"
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
	assert.Len(t, tools, 4)
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
