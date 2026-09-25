package browser

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
)

// audienceResolver is the SingleUser-capability AudienceResolver for the
// browser channel kind. The audience is always the session initiator — the
// one user driving the browser page — which is exactly clienthosted's
// shared single-user resolver; Kind.AudienceCapability / Kind.ResolveAudience
// delegate to it.
var audienceResolver = clienthosted.SingleUserAudience{Kind: KindName}

// Compile-time check: browser *Kind satisfies AudienceResolver.
var _ channelkinds.AudienceResolver = (*Kind)(nil)
