// lookup_user_for_mention is the channel-attached meta tool that resolves a user
// identifier (email, display name, or any) to the bound channel kind's native
// mention syntax. Wiring is gated on the kind's SupportedMentionLookups: a kind
// returning nil/empty makes NewLookupUserForMention return nil, and the runner
// omits the tool.
package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

const toolNameLookupUserForMention = "lookup_user_for_mention"

// LookupUserForMentionConfig wires the tool to its bound channel kind.
type LookupUserForMentionConfig struct {
	// Kind is the bound channel kind (e.g. slack). Both the resolver
	// (LookupUser) and the mention formatter (RenderMention) are called
	// through this interface.
	Kind channelkinds.Kind

	// LookupDeps carries the credentials Secret the kind needs to talk
	// to its directory API. The runner builds this from
	// resolve.ForSession.
	LookupDeps channelkinds.LookupDeps

	// ChannelKind is the bound kind's name (e.g. "slack"); used only in
	// human-readable error messages.
	ChannelKind string
}

// NewLookupUserForMention returns the tool, or nil when the bound kind
// advertises no lookup kinds. Nil returns are the wiring gate — the
// runner appends only non-nil tools to the toolset.
func NewLookupUserForMention(cfg LookupUserForMentionConfig) tool.Tool {
	if cfg.Kind == nil {
		return nil
	}
	supports := cfg.Kind.SupportedMentionLookups()
	if len(supports) == 0 {
		return nil
	}
	return &lookupUserForMentionTool{
		cfg:      cfg,
		supports: supports,
		schema:   buildSchema(supports),
		desc:     buildDescription(cfg),
	}
}

type lookupUserForMentionTool struct {
	cfg      LookupUserForMentionConfig
	supports []channelkinds.MentionLookupKind
	schema   json.RawMessage
	desc     string
}

func (t *lookupUserForMentionTool) Name() string                 { return toolNameLookupUserForMention }
func (t *lookupUserForMentionTool) Kind() tool.Kind              { return tool.KindMeta }
func (t *lookupUserForMentionTool) Description() string          { return t.desc }
func (t *lookupUserForMentionTool) InputSchema() json.RawMessage { return t.schema }
func (t *lookupUserForMentionTool) Permission() authz.Permission {
	// lookup_user_for_mention hits the bound channel's directory API
	// (e.g. Slack users.list) but the result is session-scoped —
	// callers must already have AgentSession#interact to be in the
	// session at all, and the mention token leaks no info the agent
	// doesn't already hold from the upstream owner lookup. Treated as
	// Passthrough alongside the other channel-attached meta tools
	// (respond_to_user, update_status, update_plan) until slice 2
	// formalizes a meta-tool-level check. Readonly is wrong here
	// because the Readonly path requires a resource-scoped Check and
	// this tool has no resource to bind to.
	return authz.Permission{StateImpact: authz.Passthrough}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (t *lookupUserForMentionTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *lookupUserForMentionTool) Execute(ctx context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var in struct {
		Kind  channelkinds.MentionLookupKind `json:"kind"`
		Value string                         `json:"value"`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), `{"kind":"email","value":"fred@example.com"}`); !ok {
		return res, nil
	}
	if strings.TrimSpace(in.Value) == "" {
		return tool.Result{
			Content: "lookup_user_for_mention: `value` is required and must be non-empty.",
			IsError: true,
			Trusted: true,
		}, nil
	}
	if !t.supportsKind(in.Kind) {
		return tool.Result{
			Content: fmt.Sprintf("lookup_user_for_mention: kind %q not supported on this channel; supported: [%s]",
				in.Kind, t.supportsList()),
			IsError: true,
			Trusted: true,
		}, nil
	}

	extID, _, err := t.cfg.Kind.LookupUser(ctx, t.cfg.LookupDeps, in.Kind, in.Value)
	var nearMiss *channelkinds.MentionNearMissError
	if errors.As(err, &nearMiss) {
		if len(nearMiss.Candidates) == 0 {
			// Defensive: a near-miss carrying no candidates is a plain
			// miss; degrade to the not-found shape below.
			err = channelkinds.ErrMentionNotFound
		} else {
			// Not an error result: the tool DID produce actionable
			// output, and the agent — not this code — judges whether a
			// candidate is the intended person.
			return tool.Result{
				Content: t.renderNearMiss(in.Kind, in.Value, nearMiss),
				IsError: false,
				Trusted: true,
			}, nil
		}
	}
	switch {
	case errors.Is(err, channelkinds.ErrMentionNotFound):
		return tool.Result{
			Content: fmt.Sprintf("lookup_user_for_mention: no user found in %s for %s:%q",
				t.cfg.ChannelKind, in.Kind, in.Value),
			IsError: true,
			Trusted: true,
		}, nil
	case errors.Is(err, channelkinds.ErrMentionAmbiguous):
		return tool.Result{
			Content: fmt.Sprintf("lookup_user_for_mention: multiple users matched name %q; use kind:\"email\" with a specific address",
				in.Value),
			IsError: true,
			Trusted: true,
		}, nil
	case errors.Is(err, channelkinds.ErrMentionUnsupported):
		return tool.Result{
			Content: fmt.Sprintf("lookup_user_for_mention: kind %q not supported on this channel; supported: [%s]",
				in.Kind, t.supportsList()),
			IsError: true,
			Trusted: true,
		}, nil
	case err != nil:
		return tool.Result{
			Content: fmt.Sprintf("lookup_user_for_mention: %v", err),
			IsError: true,
			Trusted: true,
		}, nil
	}

	return tool.Result{
		Content: t.cfg.Kind.RenderMention(extID),
		IsError: false,
		Trusted: true,
	}, nil
}

// renderNearMiss formats the candidate list for the model: each display
// name with its ready-to-paste mention token and (when known) its account
// standing, plus the judgement instruction. Kept as one line of prose (not
// JSON) so the guidance travels with the data. The wording differs for an
// exact-name ambiguity (several people share the name — steer toward the
// full member/employee) versus a fuzzy near-miss (approximate names — use
// a token only if clearly the same person).
func (t *lookupUserForMentionTool) renderNearMiss(
	kind channelkinds.MentionLookupKind, value string, nm *channelkinds.MentionNearMissError,
) string {
	parts := make([]string, 0, len(nm.Candidates))
	for _, c := range nm.Candidates {
		token := t.cfg.Kind.RenderMention(c.ExternalID)
		if std := strings.TrimSpace(c.AccountType); std != "" {
			parts = append(parts, fmt.Sprintf("%q (%s) → %s", c.DisplayName, std, token))
		} else {
			parts = append(parts, fmt.Sprintf("%q → %s", c.DisplayName, token))
		}
	}
	list := strings.Join(parts, "; ")
	if nm.Exact {
		return fmt.Sprintf(
			"lookup_user_for_mention: %s:%q matches multiple active users in %s: %s. "+
				"They all use that name — the intended person is most likely the full member (an active employee) over a guest. "+
				"Pick that token if it is clearly the person you mean; otherwise refer to them in plain text — do not guess.",
			kind, value, t.cfg.ChannelKind, list)
	}
	return fmt.Sprintf(
		"lookup_user_for_mention: no exact match in %s for %s:%q. Near matches: %s. "+
			"If one of these is clearly the same person, use its token verbatim; otherwise refer to them in plain text — do not guess.",
		t.cfg.ChannelKind, kind, value, list)
}

func (t *lookupUserForMentionTool) supportsKind(k channelkinds.MentionLookupKind) bool {
	for _, s := range t.supports {
		if s == k {
			return true
		}
	}
	return false
}

func (t *lookupUserForMentionTool) supportsList() string {
	parts := make([]string, 0, len(t.supports))
	for _, s := range t.supports {
		parts = append(parts, string(s))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func buildSchema(supports []channelkinds.MentionLookupKind) json.RawMessage {
	enum := make([]string, 0, len(supports))
	for _, k := range supports {
		enum = append(enum, string(k))
	}
	sort.Strings(enum)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":  map[string]any{"type": "string", "enum": enum},
			"value": map[string]any{"type": "string", "minLength": 1},
		},
		"required":             []string{"kind", "value"},
		"additionalProperties": false,
	}
	b, _ := json.Marshal(schema)
	return b
}

func buildDescription(cfg LookupUserForMentionConfig) string {
	if d := strings.TrimSpace(cfg.Kind.MentionToolDescription()); d != "" {
		return d
	}
	// Generic fallback used by kinds that don't customize. Lists the
	// supported kinds so the model sees an honest spec even without a
	// per-kind description.
	supports := cfg.Kind.SupportedMentionLookups()
	names := make([]string, 0, len(supports))
	for _, k := range supports {
		names = append(names, string(k))
	}
	sort.Strings(names)
	return fmt.Sprintf(
		"Resolve a user identifier to the %s mention syntax. Supported `kind` values: %s. "+
			"Pass {kind, value}; the tool returns the channel-native mention token (paste verbatim into respond_to_user.text) on hit, or an error on miss.",
		cfg.ChannelKind, strings.Join(names, ", "),
	)
}
