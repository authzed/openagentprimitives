package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/subjectresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// NewGetPreferences builds the get_preferences tool: the resolved per-user
// preferences snapshot for this session's class and the CURRENT turn's
// author (schema, admin globals, and the user's own saved values, already
// merged by preferences.Resolve). r is the runner's session-scoped reader;
// see PreferencesReader for why the tool depends on the interface and not a
// concrete HTTP client.
func NewGetPreferences(r PreferencesReader) tool.Tool {
	return &getPreferencesTool{reader: r}
}

type getPreferencesTool struct {
	reader PreferencesReader
}

func (*getPreferencesTool) Name() string    { return "get_preferences" }
func (*getPreferencesTool) Kind() tool.Kind { return tool.KindMeta }

// Permission: get_preferences observes session-scoped state and mutates
// nothing.
func (*getPreferencesTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*getPreferencesTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*getPreferencesTool) Description() string {
	return "List this agent's declared per-user preferences and their resolved values for the CURRENT user " +
		"(or, with `user`, a NAMED user): each key's type, description, and (for enum keys) the permitted " +
		"values with their own descriptions, plus the value now in effect, where it came from (`source`: " +
		"default, global, user, or locked), and whether admin policy has `locked` it against a user override. " +
		"Call this before assuming a default when the user's own preference would change how you respond, and " +
		"before offering to save one with set_preference so you know it is a declared key."
}

// userArgDescription renders the get_preferences `user` argument's schema
// description from subjectresolve.Usages() — the SAME registrations that
// drive Resolve's dispatch order, never a hand-copied list. A resolver added
// there without a Register call cannot land at all (Register panics on an
// undocumented Usage), and one added WITH a Register call is picked up here
// automatically, so this description can never drift out of sync with what
// the server actually accepts.
func userArgDescription() string {
	var b strings.Builder
	b.WriteString("Read a NAMED user's preferences instead of the current user's. Supported reference forms: ")
	for i, u := range subjectresolve.Usages() {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(u.Form)
		b.WriteString(" — ")
		b.WriteString(u.Description)
	}
	b.WriteString(". Omit to read the current user's preferences. Only preferences the class marks " +
		"visibility: class are returned for a named user.")
	return b.String()
}

func (*getPreferencesTool) InputSchema() json.RawMessage {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"user": map[string]any{
				"type":        "string",
				"description": userArgDescription(),
			},
		},
	}
	// A plain json.Marshal HTML-escapes '<'/'>'/'&', which would turn every
	// "<type>:<id>" form in the description into unreadable < escapes —
	// harmless to a JSON decoder, but this schema's description is prose a
	// MODEL reads, not markup a browser renders, so the escaping this default
	// guards against is not a risk here.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(schema); err != nil {
		// schema above is a static, hand-built map of strings; encoding cannot
		// fail for it. A panic here would be a programmer error, not a runtime
		// condition — same reasoning as an unreachable default case.
		panic(fmt.Sprintf("get_preferences: marshal InputSchema: %v", err))
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

type getPreferencesArgs struct {
	// User is an optional reference to a user OTHER than the current turn's
	// author — see userArgDescription for the supported forms. Empty means
	// "the current user", the tool's original and still most common shape.
	User string `json:"user,omitempty"`
}

func (t *getPreferencesTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var a getPreferencesArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"user":"trigger-author"}`); !ok {
		return res, nil
	}

	var snap preferences.SnapshotResponse
	var err error
	if a.User != "" {
		snap, err = t.reader.ForRef(ctx, a.User)
	} else {
		snap, err = t.reader.Current(ctx)
	}
	if err != nil {
		return tool.Result{
			Content: "get_preferences: " + err.Error(),
			IsError: true,
			// Framework-generated wrapper around a reader-transport error, not
			// third-party content — same reasoning as get_preferences' sibling
			// error paths across the meta package.
			Trusted: true,
		}, nil
	}

	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("get_preferences: marshal result: %w", err)
	}
	// Trusted is deliberately left false: every value in this payload is
	// user- or admin-authored content (a saved preference, an admin global),
	// never framework-controlled — the same reasoning query_memory's Content
	// documents. Meta tools bypass the PostToolCall pipeline, so leaving this
	// false is what makes the runner run the content inspectors over it.
	return tool.Result{Content: string(b)}, nil
}
