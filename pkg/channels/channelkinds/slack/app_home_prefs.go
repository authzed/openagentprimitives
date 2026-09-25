// pkg/channels/channelkinds/slack/app_home_prefs.go
//
// renderPreferenceBlocks is a PURE function: a resolved preference snapshot
// in, Block Kit blocks out. No Slack API calls, no I/O — fully unit-testable
// with plain struct assertions. Task 11 assembles these blocks into the App
// Home card (plus a per-class Save button); Task 12 decodes the action_ids
// this file mints when a user changes or saves a value.
package slack

import (
	"encoding/json"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// prefActionIDPrefix names the wire discriminator Task 12's buttoncodec
// leg matches on. The full action_id is
// "home:pref:<namespace>/<class>:<key>" — classRef carries both the
// namespace and class name so two classes of the same bare name in
// different namespaces never collide on one action_id.
const prefActionIDPrefix = "home:pref:"

// prefActionID builds "home:pref:<ns>/<class>:<key>".
//
// DECODE CONTRACT (Task 12): strip the "home:pref:" prefix, then
// strings.SplitN(rest, ":", 2) — [0] is the classRef "<ns>/<class>"
// (a Kubernetes namespace/name, so it can NEVER contain ':'), [1] is
// the key, which MAY contain ':' (UserPreferenceSchema.Name only enforces
// MinLength=1 — agentclass_types.go:538 — and preferences.ValidateSchema
// never restricts its charset). Split on the FIRST colon only. Do NOT
// strings.Split on all colons and do NOT split on the LAST colon: either
// would silently corrupt decode for a key that contains a colon.
func prefActionID(classRef, key string) string {
	return prefActionIDPrefix + classRef + ":" + key
}

// renderPreferenceBlocks renders a "Your preferences" section header followed
// by one block per resolved key. classRef is the "<namespace>/<class>" ref the
// caller already uses elsewhere — this function does not parse or re-derive it,
// only threads it into every action_id it mints. The header does NOT show the
// ref: the App Home page is already about this one agent, so naming the class
// again would be redundant and would leak its namespace to the user.
func renderPreferenceBlocks(classRef string, keys []preferences.Resolved) []slackapi.Block {
	blocks := make([]slackapi.Block, 0, len(keys)+1)
	blocks = append(blocks, slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, "*Your preferences*", false, false),
		nil, nil,
	))
	for _, k := range keys {
		blocks = append(blocks, renderPreferenceBlock(classRef, k))
	}
	return blocks
}

// renderPreferenceBlock dispatches one resolved key to its element mapping.
// Locked wins regardless of type: a locked key is always read-only.
func renderPreferenceBlock(classRef string, r preferences.Resolved) slackapi.Block {
	if r.Locked {
		return renderLockedPreferenceBlock(classRef, r)
	}
	switch r.Type {
	case "bool":
		return renderBoolPreferenceBlock(classRef, r)
	case "enum":
		return renderEnumPreferenceBlock(classRef, r)
	default:
		// "string", "int", "stringList" all render as free text; the CRD's
		// +kubebuilder:validation:Enum on UserPreferenceSchema.Type closes
		// the set to exactly these five values, so this default is the safe
		// fallback for a type this build doesn't otherwise special-case
		// rather than a reachable "unknown type" branch.
		return renderTextPreferenceBlock(classRef, r)
	}
}

// hasFreeTextPreference reports whether any UNLOCKED key in keys renders as
// a plain_text_input (string/int/stringList) — renderPreferenceBlock's own
// type switch, inverted. Bool and enum keys self-submit via dispatch_action
// on change; free-text keys don't, so Task 11's App Home card only needs a
// Save button when at least one of these is present.
func hasFreeTextPreference(keys []preferences.Resolved) bool {
	for _, k := range keys {
		if k.Locked {
			continue
		}
		switch k.Type {
		case "bool", "enum":
			continue
		default:
			return true
		}
	}
	return false
}

// renderBoolPreferenceBlock renders a single-option checkboxes element: the
// one option represents "this preference is on", checked iff the resolved
// value is true. dispatch_action is set on the containing block so a click
// round-trips immediately (Task 12), rather than waiting for a Save click.
func renderBoolPreferenceBlock(classRef string, r preferences.Resolved) *slackapi.InputBlock {
	actionID := prefActionID(classRef, r.Name)
	optText := r.Description
	if optText == "" {
		optText = r.Name
	}
	opt := slackapi.NewOptionBlockObject(
		"true",
		slackapi.NewTextBlockObject(slackapi.PlainTextType, optText, false, false),
		nil,
	)
	elem := slackapi.NewCheckboxGroupsBlockElement(actionID, opt)
	if boolValue(r) {
		elem.InitialOptions = []*slackapi.OptionBlockObject{opt}
	}

	block := slackapi.NewInputBlock(actionID, labelText(r), hintText(r), elem)
	block.DispatchAction = true
	return block
}

// renderEnumPreferenceBlock renders a static_select with one option per
// PreferenceEnumValue: Description is the option's visible text (falling
// back to Value when the class author left it blank), Value is the wire
// value. The resolved value becomes the initial option when it matches one
// of the declared choices.
func renderEnumPreferenceBlock(classRef string, r preferences.Resolved) *slackapi.InputBlock {
	actionID := prefActionID(classRef, r.Name)
	current, _ := rawValueText(r)

	options := make([]*slackapi.OptionBlockObject, 0, len(r.Enum))
	var initial *slackapi.OptionBlockObject
	for _, ev := range r.Enum {
		text := ev.Description
		if text == "" {
			text = ev.Value
		}
		opt := slackapi.NewOptionBlockObject(
			ev.Value,
			slackapi.NewTextBlockObject(slackapi.PlainTextType, text, false, false),
			nil,
		)
		options = append(options, opt)
		if ev.Value == current {
			initial = opt
		}
	}

	elem := slackapi.NewOptionsSelectBlockElement(
		slackapi.OptTypeStatic,
		slackapi.NewTextBlockObject(slackapi.PlainTextType, "Choose…", false, false),
		actionID,
		options...,
	)
	if initial != nil {
		elem.WithInitialOption(initial)
	}

	block := slackapi.NewInputBlock(actionID, labelText(r), hintText(r), elem)
	block.DispatchAction = true
	return block
}

// renderTextPreferenceBlock renders a plain_text_input for string/int/
// stringList. The Pattern (string/stringList only, per the schema) and the
// int/stringList shape are surfaced in the placeholder; the resolved value
// + its Source are surfaced in the hint. No dispatch_action here — free-text
// keys save via the per-section Save button Task 11 adds, not on every
// keystroke.
func renderTextPreferenceBlock(classRef string, r preferences.Resolved) *slackapi.InputBlock {
	actionID := prefActionID(classRef, r.Name)
	elem := slackapi.NewPlainTextInputBlockElement(
		slackapi.NewTextBlockObject(slackapi.PlainTextType, textPlaceholder(r), false, false),
		actionID,
	)
	if v, ok := rawValueText(r); ok {
		elem.WithInitialValue(v)
	}
	return slackapi.NewInputBlock(actionID, labelText(r), hintText(r), elem)
}

// renderLockedPreferenceBlock renders a read-only section for any locked
// key, regardless of type: no input element is emitted, so there is nothing
// for a user to submit and nothing for Task 12 to decode for this key.
func renderLockedPreferenceBlock(classRef string, r preferences.Resolved) *slackapi.SectionBlock {
	body := fmt.Sprintf("*%s*\n%s\n🔒 set by your administrator", labelPlain(r), preferenceValueText(r))
	blockID := fmt.Sprintf("pref_locked:%s:%s", classRef, r.Name)
	return slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, body, false, false),
		nil, nil,
		slackapi.SectionBlockOptionBlockID(blockID),
	)
}

// labelPlain/labelText give every input block a non-empty plain_text label
// (required by Slack): the key's own Description when the class author
// supplied one, else the bare key name.
func labelPlain(r preferences.Resolved) string {
	if r.Description != "" {
		return r.Description
	}
	return r.Name
}

func labelText(r preferences.Resolved) *slackapi.TextBlockObject {
	return slackapi.NewTextBlockObject(slackapi.PlainTextType, labelPlain(r), false, false)
}

// hintText shows the resolved value and where it came from, so a user
// editing a preference can see what's currently in effect and why before
// they change it.
func hintText(r preferences.Resolved) *slackapi.TextBlockObject {
	text := fmt.Sprintf("Currently: %s (%s)", preferenceValueText(r), sourceLabel(r.Source))
	return slackapi.NewTextBlockObject(slackapi.PlainTextType, text, false, false)
}

// textPlaceholder surfaces the type-specific constraint a plain_text_input
// can't otherwise express: the regex for string/stringList, the numeric
// shape for int.
func textPlaceholder(r preferences.Resolved) string {
	switch r.Type {
	case "int":
		return "enter a whole number"
	case "stringList":
		if r.Pattern != "" {
			return fmt.Sprintf("comma-separated list; each item must match pattern: %s", r.Pattern)
		}
		return "comma-separated list"
	default: // "string"
		if r.Pattern != "" {
			return fmt.Sprintf("must match pattern: %s", r.Pattern)
		}
		return "enter a value"
	}
}

// sourceLabel renders preferences.Source as the short human-readable phrase
// shown alongside a resolved value.
func sourceLabel(s preferences.Source) string {
	switch s {
	case preferences.SourceUnset:
		return "not set"
	case preferences.SourceDefault:
		return "class default"
	case preferences.SourceGlobal:
		return "admin default"
	case preferences.SourceUser:
		return "your setting"
	case preferences.SourceLocked:
		return "locked by admin"
	default:
		return string(s)
	}
}

// decodeResolvedValue JSON-decodes r.Value.Raw into a generic Go value.
// Returns ok=false when the value is unset or (defensively) unreadable —
// resolve.go only ever writes schema-validated JSON, so the latter should
// not occur in practice, but a render function degrades rather than panics
// on a value it can't parse.
func decodeResolvedValue(r preferences.Resolved) (any, bool) {
	if r.Value == nil || len(r.Value.Raw) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(r.Value.Raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// boolValue reads a bool-typed resolved value, defaulting to false when
// unset or of an unexpected shape (checkboxes render unchecked either way).
func boolValue(r preferences.Resolved) bool {
	v, ok := decodeResolvedValue(r)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// rawValueText renders any resolved value's JSON as display/wire text:
// a JSON array (stringList) joins its items with ", "; everything else
// (bool, string, number) uses its natural string form. ok is false when the
// value is unset.
func rawValueText(r preferences.Resolved) (string, bool) {
	v, ok := decodeResolvedValue(r)
	if !ok {
		return "", false
	}
	if list, isList := v.([]any); isList {
		parts := make([]string, 0, len(list))
		for _, item := range list {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, ", "), true
	}
	return fmt.Sprint(v), true
}

// preferenceValueText is rawValueText for display, with an explicit label
// for the unset case rather than an empty string.
func preferenceValueText(r preferences.Resolved) string {
	if s, ok := rawValueText(r); ok {
		return s
	}
	return "not set"
}
