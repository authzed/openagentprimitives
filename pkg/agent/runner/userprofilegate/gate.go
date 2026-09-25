// Package userprofilegate contains the shared "offer decision" helper
// internal/cmd/runner and the e2e in-process runner factory both use to decide whether
// a session's agent may see profile detail about the person speaking.
//
// One source of truth means the e2e tests exercise the production wiring path
// — if the offer decision diverges between binaries, the tests catch it. Same
// rationale, and same shape, as pkg/agent/runner/channelhistorygate.
//
// It is also the single place the user_profile capability grant is consulted.
// When it declines it returns a nil fetcher, so a Loop that was not offered
// the capability has nothing to call: "no grant means no injection" is
// structural here, not a rule someone has to remember downstream.
package userprofilegate

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// FetchFunc resolves one person's profile by their verified email. It closes
// over the bound kind and its credentials so the Loop needs neither.
type FetchFunc func(ctx context.Context, email string) (userprofile.Profile, error)

// Offer reports whether speaker-profile injection should be active for this
// session, returning the bound fetcher and the operator's field allowlist.
//
// Declines — returning (nil, nil, false) — when the AgentClass has not granted
// user_profile, has disabled it, has a config that does not parse, or when the
// bound kind cannot serve profiles. A config error declines rather than
// falling back to defaults: an operator who wrote a typo asked for something
// specific, and quietly serving a different field set would hide their mistake
// behind working-looking output. The AgentClass controller surfaces the same
// parse error as a CapabilitiesValid condition, so the operator has a signal;
// this function also logs it here (AGENTS.md: never silently drop an error),
// mirroring capability.offerContextFor's malformed-config log line. An
// ungranted capability is the ordinary case and is NOT logged — only a
// config that failed to parse is.
func Offer(
	log logr.Logger, class *spiceboxv1alpha1.AgentClass, k channelkinds.Kind, secret *corev1.Secret,
) (FetchFunc, []userprofile.Field, bool) {
	className := ""
	if class != nil {
		className = class.Name
	}
	cfg, active, err := capability.ActiveWithConfig(class, "user_profile")
	if err != nil {
		log.Info("user_profile capability config invalid; no profile injected",
			"class", className, "err", err.Error())
		return nil, nil, false
	}
	if !active {
		return nil, nil, false
	}
	upc, ok := cfg.(capability.UserProfileConfig)
	if !ok {
		return nil, nil, false
	}
	provider, ok := k.(channelkinds.UserProfileProvider)
	if !ok {
		return nil, nil, false
	}

	// Narrow the operator's allowlist to what this kind can actually populate.
	// A configured field the kind cannot supply is not an error — Slack simply
	// may not carry it — but a set with NO overlap means every fetch would
	// render an empty block, so decline rather than burn an API call per turn.
	fields := intersectFields(upc.Fields, provider.ProfileFields())
	if len(fields) == 0 {
		log.Info("user_profile: no configured field is supported by this channel kind; not offering",
			"class", className, "kind", k.Name())
		return nil, nil, false
	}

	deps := channelkinds.LookupDeps{Secret: secret}
	return func(ctx context.Context, email string) (userprofile.Profile, error) {
		return provider.FetchProfileByEmail(ctx, deps, email)
	}, fields, true
}

// intersectFields narrows configured to only the fields present in supported,
// preserving configured's order — the operator's config is the authority on
// precedence. Mirrors capability.intersectKinds (pkg/agent/tool/meta/
// capability/artifacts.go), same shape for the same reason: narrow an
// allowlist against what's actually available, in the allowlist's own order.
func intersectFields(configured, supported []userprofile.Field) []userprofile.Field {
	supportedSet := make(map[userprofile.Field]struct{}, len(supported))
	for _, f := range supported {
		supportedSet[f] = struct{}{}
	}
	var out []userprofile.Field
	for _, f := range configured {
		if _, ok := supportedSet[f]; ok {
			out = append(out, f)
		}
	}
	return out
}
