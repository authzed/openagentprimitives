package fake

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.GroupGoalHost = Kind{}

// groupFixtureKey is the External key a fake binding carries its group id in.
// The fake kind uses the base schema's `group` type, so it needs no schema
// fragment. Test-only, like the rest of this package.
const groupFixtureKey = "group"

func fakeGroupRef(b *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	if b == nil || b.External[groupFixtureKey] == "" {
		return "", false
	}
	return "group:" + b.External[groupFixtureKey] + "#member", true
}

// ConversationMemberRef implements channelkinds.GroupGoalHost.
func (Kind) ConversationMemberRef(b *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	return fakeGroupRef(b)
}

// ConversationAudienceRef implements channelkinds.GroupGoalHost; the fake's
// audience is its members.
func (Kind) ConversationAudienceRef(b *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	return fakeGroupRef(b)
}

// MembershipCurrent implements channelkinds.GroupGoalHost; the fake has no
// sync to be behind.
func (Kind) MembershipCurrent(context.Context, client.Reader, *spiceboxv1alpha1.Channel) error {
	return nil
}

// RunAnchor implements channelkinds.GroupGoalHost: a copy of b in the same
// group with a fresh fixture key, so every run starts its own session.
func (Kind) RunAnchor(b *spiceboxv1alpha1.ChannelBinding) (*spiceboxv1alpha1.ChannelBinding, error) {
	if b == nil {
		return nil, fmt.Errorf("fake RunAnchor: nil binding")
	}
	out := b.DeepCopy()
	out.Key = "group-run:" + uuid.NewString()
	return out, nil
}

// GroupMemberLinks implements channelkinds.GroupGoalHost.
func (Kind) GroupMemberLinks() []string { return []string{"group#member"} }
