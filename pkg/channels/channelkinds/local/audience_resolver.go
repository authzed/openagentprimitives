package local

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
)

// audienceResolver is the SingleUser-capability AudienceResolver for the local
// channel kind. The audience is always the session initiator — the one user
// driving the local interactive CLI/TUI — which is exactly clienthosted's shared
// single-user resolver; Kind.AudienceCapability / Kind.ResolveAudience delegate
// to it.
var audienceResolver = clienthosted.SingleUserAudience{Kind: KindName}

// Compile-time check: local *Kind satisfies AudienceResolver.
var _ channelkinds.AudienceResolver = (*Kind)(nil)
