package agentcaps

import (
	"encoding/json"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// SessionViewsCapability is the capability key granting a browser VIEW of a
// session the right to show a transcript and (per {interactions}) to talk back.
const SessionViewsCapability = "session_views"

// sessionViewsDefaultOn mirrors the capability's DefaultOn(): session_views is
// OPT-IN, so an absent grant means no view at all. Spelled once here rather than
// hardcoded into each caller's Active() call, which is the drift this resolver
// exists to prevent.
const sessionViewsDefaultOn = false

// SessionViewsConfig is the parsed value of the session_views capability: the
// interaction kinds a browser view may submit.
//
// Interactions ABSENT ⇒ NONE. `session_views: {}` grants a read-only transcript
// only. This inverts the artifacts capability's renderer-allowlist default
// deliberately — an interaction kind auto-enabling on an old `{}` grant would be
// a silent privilege grant.
type SessionViewsConfig struct {
	// Interactions is the interaction-kind allowlist; absent means read-only.
	Interactions []string `json:"interactions"`
}

// SessionViewsResolution is the resolved session_views state for one AgentClass.
//
// Active and Interactions are deliberately SEPARATE facts: "permits no browser
// interaction at all" and "permits interaction but not the kind you asked for"
// are different answers, reported as different 403 messages. Collapsing them
// into one empty slice would merge those messages.
type SessionViewsResolution struct {
	// Active reports the capability granted AND enabled on the class.
	Active bool
	// Interactions is the granted interaction-kind list. Nil when the
	// capability is inactive, and also nil when it is granted read-only
	// (`session_views: {}`) — see SessionViewsConfig.
	Interactions []string
}

// ResolveSessionViews resolves the session_views capability for class: whether
// it is active, and which interaction kinds it grants.
//
// Read from spec, never status (see the package doc), so a revocation takes
// effect immediately.
//
// A malformed grant envelope or config value returns the ZERO resolution AND a
// non-nil error, so no caller can act on a half-parsed value. Every caller must
// FAIL CLOSED on that error and log it.
//
// A nil class, an absent grant, or an explicitly disabled one is NOT an error:
// it is the ordinary "no browser interaction here" answer, {Active: false}.
func ResolveSessionViews(class *spiceboxv1alpha1.AgentClass) (SessionViewsResolution, error) {
	grant, err := GrantOf(class, SessionViewsCapability)
	if err != nil {
		return SessionViewsResolution{}, err
	}
	if !Active(sessionViewsDefaultOn, grant) {
		return SessionViewsResolution{}, nil
	}
	cfg, err := ParseSessionViewsConfig(grant.Raw)
	if err != nil {
		return SessionViewsResolution{}, err
	}
	return SessionViewsResolution{Active: true, Interactions: cfg.Interactions}, nil
}

// ParseSessionViewsConfig decodes a raw session_views capability value. An empty
// value yields the zero config (granted, read-only) with no error; a malformed
// one yields the zero config AND an error, never a partially-populated config.
//
// Exported separately from ResolveSessionViews because the capability registry's
// ParseConfig validates a raw value with no AgentClass in hand — which is how the
// AgentClass controller rejects a bad {interactions} at admission.
func ParseSessionViewsConfig(raw json.RawMessage) (SessionViewsConfig, error) {
	if len(raw) == 0 {
		return SessionViewsConfig{}, nil
	}
	var cfg SessionViewsConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return SessionViewsConfig{}, err
	}
	return cfg, nil
}
