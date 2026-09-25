// metaagentHandlers.dispatch derives the sub-channel name from the
// channelevents.Kind the subject addresses — string(kind) — instead of taking
// it as a second, independently-written parameter. That is only sound while the
// two spellings agree, and they are declared in different packages for
// different audiences (the wire subject vs. the Kind.SubChannelSender key), so
// nothing but this test stops one from being renamed alone.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestMetaagentSubChannelNamesMatchTheirKinds(t *testing.T) {
	assert.Equal(t, channelkinds.SubChannelMetaagentScopeApproval,
		string(channelevents.KindMetaagentScopeApproval),
		"the scope-approval sub-channel key and its wire kind must stay one string")
	assert.Equal(t, channelkinds.SubChannelMetaagentNotice,
		string(channelevents.KindMetaagentNotice),
		"the notice sub-channel key and its wire kind must stay one string")
}
