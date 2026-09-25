package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// NewSetPreference builds the set_preference tool: saves (or clears) one
// declared per-user preference for the CURRENT turn's author, after a
// human confirms the exact save. r resolves the current schema + value to
// validate and confirm against; s publishes the confirm and blocks for the
// decision. Both are the runner's session-scoped adapters — see
// PreferencesReader / PreferenceSaver.
func NewSetPreference(r PreferencesReader, s PreferenceSaver) tool.Tool {
	return &setPreferenceTool{reader: r, saver: s}
}

type setPreferenceTool struct {
	reader PreferencesReader
	saver  PreferenceSaver
}

func (*setPreferenceTool) Name() string    { return "set_preference" }
func (*setPreferenceTool) Kind() tool.Kind { return tool.KindMeta }

// Permission: set_preference never writes anything itself — the commit, if
// any, happens on channelsd's preference_save decision handler once the
// human approves — so from the runner's own state-impact accounting this
// call is stateless. The human-in-the-loop confirm is the actual gate.
func (*setPreferenceTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*setPreferenceTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*setPreferenceTool) Description() string {
	return "Save (or clear) one of this agent's declared per-user preferences for the CURRENT user, from " +
		"get_preferences. Every save asks the user to confirm the exact change first — this call blocks until " +
		"they answer or the confirm times out — so only call it once you have a specific value the user actually " +
		"wants saved, not speculatively. A key locked by admin policy cannot be changed. Omitting `value` (or " +
		"passing JSON null) clears the user's saved value so the admin/default applies again."
}

func (*setPreferenceTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"required":["key"],
		"properties":{
			"key":{"type":"string","description":"The preference to save, from get_preferences."},
			"value":{"description":"The value to save for the current user, matching the preference's declared type. JSON null (or omitting value) clears the user's saved value so the admin/default applies again."}
		}
	}`)
}

type setPreferenceArgs struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value,omitempty"`
}

func (t *setPreferenceTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var a setPreferenceArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"key":"language","value":"de"}`); !ok {
		return res, nil
	}

	snap, err := t.reader.Current(ctx)
	if err != nil {
		return setPreferenceError(err.Error()), nil
	}

	found, ok := findResolved(snap, a.Key)
	if !ok {
		return setPreferenceError(fmt.Sprintf("unknown preference %q. Known preferences: %s.",
			a.Key, strings.Join(knownPreferenceNames(snap), ", "))), nil
	}

	if found.Locked {
		return setPreferenceError(fmt.Sprintf("%q is locked by admin policy", a.Key)), nil
	}

	clearing := isClearValue(a.Value)
	var argValue apiextv1.JSON
	if !clearing {
		argValue = apiextv1.JSON{Raw: a.Value}
	}

	schema := spiceboxv1alpha1.UserPreferenceSchema{
		Name:    found.Name,
		Type:    found.Type,
		Enum:    found.Enum,
		Pattern: found.Pattern,
	}
	if err := preferences.ValidateValue(schema, argValue); err != nil {
		return setPreferenceError(fmt.Sprintf("%q: %v", a.Key, err)), nil
	}

	display := preferenceSaveDisplay(a.Key, found, clearing, a.Value)

	var valuePtr *apiextv1.JSON
	if !clearing {
		valuePtr = &argValue
	}

	outcome, err := t.saver.Save(ctx, a.Key, valuePtr, display)
	if err != nil {
		return setPreferenceError(err.Error()), nil
	}

	switch {
	case outcome.Approved:
		return t.reportSaved(ctx, a.Key, outcome.DecidedBy), nil
	case outcome.Denied:
		return tool.Result{Content: "not saved: the user declined.", IsError: false}, nil
	case outcome.TimedOut:
		return tool.Result{Content: "not saved: confirmation timed out.", IsError: false}, nil
	default:
		// SaveOutcome's contract is exactly one of the three set whenever Save
		// returns a nil error; this is a saver bug, not a user-facing outcome.
		// Never silently swallow it — see AGENTS.md.
		return setPreferenceError(fmt.Sprintf("saver returned no decision for %q", a.Key)), nil
	}
}

// reportSaved re-reads the resolved snapshot after an approved save and
// reports the key's new value and source plainly. The commit already
// happened by the time Save returned — this is a readback, not a write.
func (t *setPreferenceTool) reportSaved(ctx context.Context, key, decidedBy string) tool.Result {
	snap, err := t.reader.Current(ctx)
	if err != nil {
		return tool.Result{
			Content: fmt.Sprintf("set_preference: saved, but could not confirm the new value: %v", err),
			IsError: true,
			Trusted: true,
		}
	}
	found, ok := findResolved(snap, key)
	if !ok {
		return tool.Result{
			Content: fmt.Sprintf("set_preference: saved, but %q no longer appears in the resolved snapshot", key),
			IsError: true,
			Trusted: true,
		}
	}
	return tool.Result{
		Content: fmt.Sprintf("saved: %s = %s (source: %s), confirmed by %s",
			key, renderResolvedValue(found), string(found.Source), decidedBy),
	}
}

func setPreferenceError(msg string) tool.Result {
	return tool.Result{
		Content: "set_preference: " + msg,
		IsError: true,
		// Framework-generated refusal/validation text (an unknown-key list
		// pulled from the class's own declared schema, a lock notice, a local
		// validator's message, or a transport error) — not third-party content.
		Trusted: true,
	}
}

func findResolved(snap preferences.SnapshotResponse, key string) (preferences.Resolved, bool) {
	for _, k := range snap.Snapshot.Keys {
		if k.Name == key {
			return k, true
		}
	}
	return preferences.Resolved{}, false
}

func knownPreferenceNames(snap preferences.SnapshotResponse) []string {
	names := make([]string, 0, len(snap.Snapshot.Keys))
	for _, k := range snap.Snapshot.Keys {
		names = append(names, k.Name)
	}
	return names
}

// isClearValue reports whether the tool-call's raw value argument means
// "clear the user's saved value" — either omitted entirely or an explicit
// JSON null.
func isClearValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == "null"
}

// preferenceSaveDisplay is the exact approver-facing sentence the confirm
// card shows. It is deliberately built from the human-readable rendering of
// the value (renderHumanValue), not its raw JSON — "language: de", not
// `language: "de"` — since the person confirming reads it as prose.
func preferenceSaveDisplay(key string, found preferences.Resolved, clearing bool, raw json.RawMessage) string {
	if clearing {
		return fmt.Sprintf("Clear your saved %q?", key)
	}
	return fmt.Sprintf("Save %q as your default for this agent?", key+": "+renderHumanValue(found, raw))
}

// renderHumanValue formats a not-yet-saved argument value for display in
// the confirm prompt, decoding per the preference's declared type so a
// string reads bare ("de") rather than JSON-quoted (`"de"`). Falls back to
// the raw JSON text (trimmed) for a value that fails to decode as its
// declared type — ValidateValue has already run by the time this is called
// in the ordinary path, so that fallback is defense in depth, not the
// common case.
//
// The decoded (or fallback) text is MODEL-CHOSEN — the agent's own free-text
// value, not something this package authored — so before it reaches a human
// via the confirm card it is sanitized here: sanitizeHumanValue collapses
// control characters (including newlines) to single spaces and caps the
// result at maxHumanValueRunes runes. This is the ONE choke point for that
// sanitization per the cap-the-artifact rule; preferenceSaveDisplay and the
// runner's preferenceSaveLead (which renders this string as the confirm
// card's Lead) both consume the already-sanitized output and must not
// re-embed the raw value. The wire `value` committed on approval is a
// separate, unsanitized field — only this human-facing rendering is capped.
func renderHumanValue(found preferences.Resolved, raw json.RawMessage) string {
	return sanitizeHumanValue(decodeHumanValue(found, raw))
}

// decodeHumanValue does the type-directed decode renderHumanValue documents;
// split out so the sanitization step in renderHumanValue applies uniformly
// to every return path, including the raw-JSON fallback.
func decodeHumanValue(found preferences.Resolved, raw json.RawMessage) string {
	switch found.Type {
	case "string", "enum":
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	case "stringList":
		var items []string
		if err := json.Unmarshal(raw, &items); err == nil {
			return strings.Join(items, ", ")
		}
	case "int":
		var n int64
		if err := json.Unmarshal(raw, &n); err == nil {
			return strconv.FormatInt(n, 10)
		}
	case "bool":
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return strconv.FormatBool(b)
		}
	}
	return strings.TrimSpace(string(raw))
}

// maxHumanValueRunes caps how much of a model-chosen value the confirm card
// renders before truncating with "…" — the cap-the-artifact bound for this
// one choke point (sanitizeHumanValue), not a limit on the saved value
// itself.
const maxHumanValueRunes = 120

// sanitizeHumanValue makes a model-chosen value safe to render verbatim in a
// human-facing confirm card: every control character (including \n and \r)
// collapses to a single space — consecutive control/space runs collapse to
// one space, so a multiline value reads as one line — and the result is
// capped at maxHumanValueRunes runes, with a "…" suffix when truncated.
func sanitizeHumanValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	spacePending := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			spacePending = true
			continue
		}
		if spacePending {
			if b.Len() > 0 {
				b.WriteRune(' ')
			}
			spacePending = false
		}
		b.WriteRune(r)
	}
	out := b.String()

	runes := []rune(out)
	if len(runes) > maxHumanValueRunes {
		return string(runes[:maxHumanValueRunes]) + "…"
	}
	return out
}

// renderResolvedValue formats an already-resolved preference's value for the
// plain post-save report, as its raw JSON text (e.g. `"de"`, `5`, `true`) —
// unlike renderHumanValue, this reads as a value dump, not prose, so the
// quoting that distinguishes a string from a number is exactly what the
// caller wants to see. Nil (SourceUnset — nothing resolved, e.g. right after
// a clear with no default or global to fall back to) renders as "(unset)".
func renderResolvedValue(r preferences.Resolved) string {
	if r.Value == nil {
		return "(unset)"
	}
	return strings.TrimSpace(string(r.Value.Raw))
}
