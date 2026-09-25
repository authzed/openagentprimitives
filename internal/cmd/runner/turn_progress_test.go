package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestBuildLoopTurnProgress_NilWhenNotPublishable(t *testing.T) {
	// No InputChannel → not channel-attached → nil hook.
	noChannel := &spiceboxv1alpha1.AgentSession{}
	assert.Nil(t, buildLoopTurnProgress(true, nil, noChannel, nil), "no channel binding → nil")
	assert.Nil(t, buildLoopTurnProgress(false, nil, noChannel, nil), "not chanAttached → nil")
}

func TestBuildLoopToolProgress_NilWhenNotPublishable(t *testing.T) {
	// No InputChannel → not channel-attached → nil hook.
	noChannel := &spiceboxv1alpha1.AgentSession{}
	assert.Nil(t, buildLoopToolProgress(true, nil, noChannel, nil), "no channel binding → nil")
	assert.Nil(t, buildLoopToolProgress(false, nil, noChannel, nil), "not chanAttached → nil")
}
