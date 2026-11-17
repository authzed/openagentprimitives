package slack

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.GroupGoalHost = (*Kind)(nil)

// groupConversationChannelID returns the Slack channel id of a binding that
// addresses a multi-person conversation, or "" for anything else.
//
// The rule is by id prefix, matching the package's existing DM test
// (thread_title.go): "D" is a 1:1 DM; "G" is a group DM (mpim) or legacy
// private group. Slack has no stricter classifier available without an API
// call, so only "C…" conversations are treated as groups. A private channel
// with a "G" id is therefore refused too: that fails closed (no group goal
// there) rather than minting ownership over a conversation whose membership we
// cannot vouch for.
func groupConversationChannelID(b *spiceboxv1alpha1.ChannelBinding) string {
	id := slackChannelIDFromBinding(b)
	if id == "" || strings.HasPrefix(id, "D") || strings.HasPrefix(id, "G") {
		return ""
	}
	return id
}

// ConversationMemberRef implements channelkinds.GroupGoalHost:
// slack_channel:<id>#member for a channel conversation, ok=false for DMs and
// group DMs.
func (k *Kind) ConversationMemberRef(b *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	id := groupConversationChannelID(b)
	if id == "" {
		return "", false
	}
	return "slack_channel:" + id + "#member", true
}

// ConversationAudienceRef implements channelkinds.GroupGoalHost by delegating
// to ChannelViewSubjectRef, after the same group-conversation filter.
func (k *Kind) ConversationAudienceRef(b *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	if groupConversationChannelID(b) == "" {
		return "", false
	}
	return k.ChannelViewSubjectRef(b)
}

// MembershipCurrent implements channelkinds.GroupGoalHost. It returns nil iff a
// RelationshipSource with spec.kind "slack" exists in the Channel's namespace
// and reports Ready=True.
//
// This is COARSE BY DESIGN: a RelationshipSource carries no workspace identity
// (and neither does a Channel without a Slack API call), so it cannot be
// matched to the Channel's workspace. A mismatched workspace fails closed —
// its members never resolve, so membership checks refuse — rather than
// open.
func (k *Kind) MembershipCurrent(ctx context.Context, r client.Reader, ch *spiceboxv1alpha1.Channel) error {
	if ch == nil {
		return fmt.Errorf("slack MembershipCurrent: nil Channel")
	}
	var list spiceboxv1alpha1.RelationshipSourceList
	if err := r.List(ctx, &list, client.InNamespace(ch.Namespace)); err != nil {
		return fmt.Errorf("slack MembershipCurrent: list RelationshipSources in namespace %s: %w", ch.Namespace, err)
	}
	var notReady string
	for i := range list.Items {
		s := &list.Items[i]
		if s.Spec.Kind != KindName {
			continue
		}
		c := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.RelationshipSourceConditionReady)
		if c != nil && c.Status == metav1.ConditionTrue {
			return nil
		}
		if notReady == "" {
			reason := "no Ready condition yet"
			if c != nil {
				reason = c.Reason
			}
			notReady = fmt.Sprintf("Slack directory sync (RelationshipSource) %s/%s is not Ready: %s", s.Namespace, s.Name, reason)
		}
	}
	if notReady != "" {
		return fmt.Errorf("%s", notReady)
	}
	return fmt.Errorf("no ready Slack directory sync (RelationshipSource) in namespace %s", ch.Namespace)
}

// RunAnchor implements channelkinds.GroupGoalHost: a copy of b rooted at the
// channel itself ("channel:<id>", the bare anchor OutboundAnchor also uses), so
// the first send roots a fresh thread. thread_ts and the thread permalink are
// dropped; everything else (channel_id, team_id) is kept. b is not mutated.
func (k *Kind) RunAnchor(b *spiceboxv1alpha1.ChannelBinding) (*spiceboxv1alpha1.ChannelBinding, error) {
	id := slackChannelIDFromBinding(b)
	if id == "" {
		return nil, fmt.Errorf("slack RunAnchor: binding addresses no channel")
	}
	out := b.DeepCopy()
	out.Key = "channel:" + id
	if out.External == nil {
		out.External = map[string]string{}
	}
	out.External["channel_id"] = id
	delete(out.External, "thread_ts")
	delete(out.External, "permalink")
	return out, nil
}

// GroupMemberLinks implements channelkinds.GroupGoalHost: the subject-set
// type of ConversationMemberRef only. slack_channel#view is the AUDIENCE ref
// and is deliberately absent — composed into agent_goal_domain#member_group it
// would let a channel's whole audience (for a public channel, the workspace)
// be written in as goal owners.
func (k *Kind) GroupMemberLinks() []string {
	return []string{"slack_channel#member"}
}
