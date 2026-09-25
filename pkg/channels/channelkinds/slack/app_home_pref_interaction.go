// pkg/channels/channelkinds/slack/app_home_pref_interaction.go
//
// handleHomePrefClick is the App Home preference-EDIT path: a user changes
// a control (checkbox/select) or clicks Save on their Home tab, this decodes
// the click, commits the new value through the first-party preferences
// endpoint, and republishes the tab so the user sees the result immediately.
//
// THE SECURITY PROPERTY: every commit's Subject is
// resolveCanonicalForSlackUser(ctx, cb.User.ID) (bare form) — cb.User.ID is
// the field Slack itself populates naming who clicked, verified upstream by
// socket-mode's own signature check before this code ever runs. Nothing in
// this file reads any OTHER field off cb (ActionCallback.BlockActions[*],
// View.State, ResponseURL, …) as a subject. There is no "subject" or "user"
// field in the block-action wire shape this code decodes — the action_id
// carries only classRef+key (see app_home_prefs.go's DECODE CONTRACT) and
// the value carries only the new setting — so a client crafting an
// arbitrary payload has no field to put a different subject INTO; the write
// subject is computed, never read off the wire.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	slackapi "github.com/slack-go/slack"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// prefSaveActionIDPrefix names the Save button's action_id prefix
// app_home_view.go mints ("home:pref_save:"+classRef — no per-key encoding,
// since the button commits every free-text field currently shown for that
// class in one click).
const prefSaveActionIDPrefix = "home:pref_save:"

// handleHomePrefClick recognizes the two App Home preference-edit
// interactions and returns (false, nil) for anything else so onInteraction
// falls through to the next discriminator:
//
//   - a "home:pref:" control change (bool checkbox / enum static_select,
//     both self-submitting via dispatch_action) — handleHomePrefControlChange.
//   - a "home:pref_save:" button (commits every free-text field currently
//     shown for that class from the view's own state) — handleHomePrefSave.
//
// Both paths commit through CommitPreferenceFirstParty with the CLICKER'S
// OWN canonical subject and then republish the Home tab, win or lose, so a
// failed commit doesn't leave a control showing an unsaved optimistic value.
func (l *slackListener) handleHomePrefClick(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	if cb.Type != slackapi.InteractionTypeBlockActions || len(cb.ActionCallback.BlockActions) == 0 {
		return false, nil
	}
	action := cb.ActionCallback.BlockActions[0]
	switch {
	case strings.HasPrefix(action.ActionID, prefActionIDPrefix):
		return l.handleHomePrefControlChange(ctx, cb, action)
	case strings.HasPrefix(action.ActionID, prefSaveActionIDPrefix):
		return l.handleHomePrefSave(ctx, cb, action)
	default:
		return false, nil
	}
}

// handleHomePrefControlChange handles one self-submitting control (a bool
// checkbox or an enum static_select) changing value. Decodes classRef+key
// from the action_id per app_home_prefs.go's DECODE CONTRACT, reads the new
// value off the SAME action's own typed fields (SelectedOptions /
// SelectedOption — never a free-form payload field), resolves the clicker's
// own bare canonical, commits, and republishes.
func (l *slackListener) handleHomePrefControlChange(ctx context.Context, cb slackapi.InteractionCallback, action *slackapi.BlockAction) (bool, error) {
	logger := log.FromContext(ctx)

	classRef, key, ok := decodePrefActionID(action.ActionID)
	if !ok {
		return true, fmt.Errorf("home pref click: malformed action_id %q", action.ActionID)
	}
	ns, className, ok := strings.Cut(classRef, "/")
	if !ok || ns == "" || className == "" {
		return true, fmt.Errorf("home pref click: malformed class ref %q", classRef)
	}
	value, err := decodeControlValue(action)
	if err != nil {
		return true, fmt.Errorf("home pref click: decode value for action_id %q: %w", action.ActionID, err)
	}

	bareCanonical, err := l.clickerBareCanonical(ctx, cb.User.ID)
	if err != nil {
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Couldn't verify your identity — please try again.")
		return true, fmt.Errorf("home pref click: %w", err)
	}
	if l.preferences == nil {
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Preferences aren't available right now.")
		return true, fmt.Errorf("home pref click: no preferences client configured")
	}

	commitErr := l.preferences.CommitPreferenceFirstParty(ctx, ns, className, preferences.CommitRequest{
		Key:     key,
		Value:   value,
		Subject: bareCanonical,
	})
	// Republish regardless of outcome: a successful commit shows the new
	// state confirmed; a failed one reverts a client-optimistic toggle back
	// to what's actually stored, so the control never shows an unsaved lie.
	l.republishHomeTab(ctx, cb.User.ID)

	if commitErr != nil {
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Couldn't save your preference — please try again.")
		return true, fmt.Errorf("home pref click: commit %s/%s key %q: %w", ns, className, key, commitErr)
	}
	logger.Info("slack: app home preference committed",
		"namespace", ns, "class", className, "key", key, "clicker", cb.User.ID)
	return true, nil
}

// handleHomePrefSave handles the per-class Save button: it commits every
// UNLOCKED free-text (string/int/stringList) key currently rendered for
// classRef, reading each one's live value out of the view's own state
// (cb.View.State.Values) rather than any button-value payload — the Save
// button's own value/action_id carry only classRef, nothing per-key.
//
// Key TYPES (needed to encode the raw text correctly — an int must become a
// JSON number, not a quoted string) come from GetPreferencesFirstParty's
// resolved snapshot for the SAME clicker, not from the click payload.
func (l *slackListener) handleHomePrefSave(ctx context.Context, cb slackapi.InteractionCallback, action *slackapi.BlockAction) (bool, error) {
	logger := log.FromContext(ctx)

	classRef := strings.TrimPrefix(action.ActionID, prefSaveActionIDPrefix)
	ns, className, ok := strings.Cut(classRef, "/")
	if !ok || ns == "" || className == "" {
		return true, fmt.Errorf("home pref save: malformed class ref %q", classRef)
	}

	bareCanonical, err := l.clickerBareCanonical(ctx, cb.User.ID)
	if err != nil {
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Couldn't verify your identity — please try again.")
		return true, fmt.Errorf("home pref save: %w", err)
	}
	if l.preferences == nil {
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Preferences aren't available right now.")
		return true, fmt.Errorf("home pref save: no preferences client configured")
	}

	snap, err := l.preferences.GetPreferencesFirstParty(ctx, ns, className, bareCanonical)
	if err != nil {
		l.republishHomeTab(ctx, cb.User.ID)
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Couldn't load your preferences — please try again.")
		return true, fmt.Errorf("home pref save: get preferences for %s/%s: %w", ns, className, err)
	}
	types := make(map[string]string, len(snap.Snapshot.Keys))
	for _, k := range snap.Snapshot.Keys {
		types[k.Name] = k.Type
	}

	// Collect the block_ids in scope for this class deterministically (map
	// iteration order is random) so commit order — and therefore which
	// commit "wins" the surfaced error on a partial failure — is stable
	// across runs.
	prefix := prefActionIDPrefix + classRef + ":"
	var blockIDs []string
	if cb.View.State != nil {
		for blockID := range cb.View.State.Values {
			if strings.HasPrefix(blockID, prefix) {
				blockIDs = append(blockIDs, blockID)
			}
		}
	}
	sort.Strings(blockIDs)

	var commitErr error
	var committedKey string
	for _, blockID := range blockIDs {
		_, key, ok := decodePrefActionID(blockID)
		if !ok {
			continue
		}
		typ, declared := types[key]
		if !declared {
			// Not on the current schema (stale button render, or the class
			// changed shape since this tab was published) — skip rather than
			// guess a shape the server's ValidateValue would reject anyway.
			logger.Info("slack: app home pref save skipping undeclared key",
				"namespace", ns, "class", className, "key", key)
			continue
		}
		if typ == "bool" || typ == "enum" {
			// Self-submitting controls; the Save button never carries these —
			// a stray match here would mean a render/decode mismatch.
			continue
		}
		fieldAction, ok := cb.View.State.Values[blockID][blockID]
		if !ok {
			continue
		}
		value, verr := freeTextValue(typ, fieldAction.Value)
		if verr != nil {
			commitErr = fmt.Errorf("key %q: %w", key, verr)
			committedKey = key
			break
		}
		if cerr := l.preferences.CommitPreferenceFirstParty(ctx, ns, className, preferences.CommitRequest{
			Key:     key,
			Value:   value,
			Subject: bareCanonical,
		}); cerr != nil {
			commitErr = fmt.Errorf("key %q: %w", key, cerr)
			committedKey = key
			break
		}
		committedKey = key
	}

	l.republishHomeTab(ctx, cb.User.ID)

	if commitErr != nil {
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Couldn't save your preferences — please try again.")
		return true, fmt.Errorf("home pref save: %s/%s: %w", ns, className, commitErr)
	}
	logger.Info("slack: app home preference save committed",
		"namespace", ns, "class", className, "lastKey", committedKey, "clicker", cb.User.ID)
	return true, nil
}

// clickerBareCanonical resolves cb.User.ID — the Slack-verified field naming
// WHO CLICKED — to the bare canonical CommitPreferenceFirstParty's Subject
// wants. This is the ONLY place either handler above derives a subject;
// mirrors buildHomeInputForUser's own derivation (app_home.go) so a
// preference read and a preference write key on exactly the same identity.
func (l *slackListener) clickerBareCanonical(ctx context.Context, slackUserID string) (string, error) {
	canonical, err := l.resolveCanonicalForSlackUser(ctx, slackUserID)
	if err != nil {
		return "", fmt.Errorf("resolve clicker canonical: %w", err)
	}
	bare, err := identity.Subject(canonical).CanonicalUserID()
	if err != nil {
		return "", fmt.Errorf("clicker canonical has no bare user id: %w", err)
	}
	return bare.String(), nil
}

// republishHomeTab re-publishes the Home tab for slackUserID, mirroring
// handleAppHomeOpened's own build+publish sequence (app_home.go), so the tab
// always reflects the operator's authoritative state right after a
// preference edit — whether that edit succeeded or failed. Best-effort: a
// failure here only logs; the click's own success/failure has already been
// decided independently by the caller.
func (l *slackListener) republishHomeTab(ctx context.Context, slackUserID string) {
	logger := log.FromContext(ctx)
	in, err := l.buildHomeInputForUser(ctx, slackUserID)
	if err != nil {
		logger.Info("app_home: republish after pref click: build input failed",
			"user", slackUserID, "err", err.Error())
		in = appHomeViewInput{}
	}
	view := buildAppHomeView(in)
	req := slackapi.PublishViewContextRequest{UserID: slackUserID, View: view}
	if _, err := l.api.PublishViewContext(ctx, req); err != nil {
		logger.Info("app_home: republish after pref click: views.publish failed",
			"user", slackUserID, "err", err.Error())
	}
}

// decodePrefActionID implements app_home_prefs.go's DECODE CONTRACT: strip
// the "home:pref:" prefix, then split on the FIRST colon only. parts[0] is
// classRef ("<ns>/<class>", never contains ':' — a Kubernetes namespace/name
// pair); parts[1] is the key, which MAY itself contain ':'. Splitting on the
// last colon or on every colon would silently corrupt decode for a key that
// contains one.
func decodePrefActionID(actionID string) (classRef, key string, ok bool) {
	rest, hasPrefix := strings.CutPrefix(actionID, prefActionIDPrefix)
	if !hasPrefix {
		return "", "", false
	}
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// decodeControlValue reads the new value off a self-submitting control's OWN
// typed fields — never off action.Value, which for these two element types
// is not even populated by Slack. Nil is the tombstone/clear wire shape
// CommitRequest documents; an empty static_select selection maps to it, a
// checkbox never does (unchecked IS "false", not "unset").
func decodeControlValue(action *slackapi.BlockAction) (*apiextv1.JSON, error) {
	switch action.Type {
	case slackapi.ActionType(slackapi.METCheckboxGroups):
		return jsonBool(len(action.SelectedOptions) > 0), nil
	case slackapi.ActionType(slackapi.OptTypeStatic):
		v := action.SelectedOption.Value
		if v == "" {
			return nil, nil
		}
		return jsonString(v), nil
	default:
		return nil, fmt.Errorf("unsupported control type %q", action.Type)
	}
}

// freeTextValue encodes a plain_text_input's raw text per the key's declared
// schema type, matching preferences.ValidateValue's own expected JSON shape
// per type: "string" -> a JSON string, "int" -> a JSON number, "stringList"
// -> a JSON array of strings (comma-separated in the box). Blank (after
// trimming) is the clear/tombstone wire shape (nil) for every type.
func freeTextValue(schemaType, raw string) (*apiextv1.JSON, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	switch schemaType {
	case "int":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected a whole number, got %q: %w", raw, err)
		}
		enc, _ := json.Marshal(n)
		return &apiextv1.JSON{Raw: enc}, nil
	case "stringList":
		fields := strings.Split(raw, ",")
		items := make([]string, 0, len(fields))
		for _, f := range fields {
			if f = strings.TrimSpace(f); f != "" {
				items = append(items, f)
			}
		}
		if len(items) == 0 {
			return nil, nil
		}
		enc, _ := json.Marshal(items)
		return &apiextv1.JSON{Raw: enc}, nil
	default: // "string"
		return jsonString(raw), nil
	}
}

// jsonBool/jsonString build a *apiextv1.JSON from a Go value that can never
// fail to marshal (a bool, a valid Go string), so both stay error-free for
// callers that already know their input is well-formed.
func jsonBool(b bool) *apiextv1.JSON {
	raw, _ := json.Marshal(b)
	return &apiextv1.JSON{Raw: raw}
}

func jsonString(s string) *apiextv1.JSON {
	raw, _ := json.Marshal(s)
	return &apiextv1.JSON{Raw: raw}
}
