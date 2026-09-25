package slack

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// spiceDBLookup is the narrow interface needed for audience resolution.
// channelkinds.AuthzReader satisfies it — *pkg/authz/spicedb.Client implements
// AuthzReader and parses the "<type>:<id>#<relation>" subjectRef form used
// by LookupSubjects, so passing "slack_channel:<id>#view" works directly.
type spiceDBLookup interface {
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)
}

type audienceResolver struct {
	sdb spiceDBLookup
}

func newAudienceResolverForTest(sdb spiceDBLookup) *audienceResolver {
	return &audienceResolver{sdb: sdb}
}

// AudienceCapability is a static property; no I/O.
func (a *audienceResolver) AudienceCapability() channelkinds.Capability {
	return channelkinds.CapabilityFull
}

// ResolveAudience looks up all subjects with view on slack_channel:<channelID>.
func (a *audienceResolver) ResolveAudience(ctx context.Context, sess channelkinds.SessionInfo) ([]string, error) {
	if sess.Channel == nil {
		return nil, fmt.Errorf("slack audience: Channel binding missing")
	}
	channelID := sess.Channel.External["channel_id"]
	if channelID == "" {
		return nil, fmt.Errorf("slack audience: channel_id missing from Channel.External")
	}
	return a.sdb.LookupSubjects(ctx, "slack_channel:"+channelID+"#view")
}

// Compile-time assertion: *Kind satisfies channelkinds.AudienceResolver.
var _ channelkinds.AudienceResolver = (*Kind)(nil)
