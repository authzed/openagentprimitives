// pkg/channels/channelkinds/slack/manifest.go
//
// Slack-app manifest YAML the wizard shows when the user doesn't yet have
// a Slack app, plus the copy of it the wizard leaves on disk. The user pastes
// this at https://api.slack.com/apps → "Create New App" → "From an app
// manifest" to bootstrap the bot with the correct scopes + event subscriptions
// for the agentprimitives integration.
package slack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// defaultBotDisplayName is the app + bot display name used when the wizard has
// no agent to derive a name from (e.g. the monitoring flow, or a fallback).
const defaultBotDisplayName = "agentprimitives-bot"

// manifestFileStem and manifestFileExt bracket the name of the copy the wizard
// leaves in the working directory. The agent's name goes between them, so two
// agents set up from the same directory do not overwrite each other.
const (
	manifestFileStem = "slack-app-manifest-"
	manifestFileExt  = ".yaml"
	// manifestFilePerm is deliberately world-readable: the manifest names
	// scopes and an app title, and carries no credential of any kind.
	manifestFilePerm = 0o644
)

// manifestNameLimit is Slack's cap on display_information.name. A manifest
// naming an app past it is refused outright (invalid_manifest), so a name that
// would exceed it is cut here rather than sent.
const manifestNameLimit = 35

// foldName folds everything outside [A-Za-z0-9_-] to '-' and trims the
// leading and trailing '-'. The result is pure ASCII, so its length in bytes
// and in characters are the same thing.
//
// Shared by the two places a caller's name is used unedited-looking and must
// not be: the FILE the manifest is written to, where a '/' or a ".." chooses
// the directory it lands in, and the NAME inside the document, where Slack
// accepts a narrower alphabet than the names this project's own validators
// do. An empty result is the caller's problem to fall back from — the two
// differ on what the fallback is worth.
func foldName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// manifestFileName names the saved manifest after whatever the app was
// generated for — the AgentClass from the channel wizard, the AgentIdentity
// from the credential-setup flow. Anything outside the safe set is folded to
// '-' rather than trusted: the name reaches here from a flag on an offline
// run, where no cluster lookup has had the chance to reject it, and a '/' or
// a ".." in it would otherwise choose the directory the file lands in.
//
// NOT length-capped, unlike the display name inside the document: a long file
// name is merely long, and cutting it is how two agents whose names share a
// prefix come to overwrite each other's copy.
func manifestFileName(subject string) string {
	name := foldName(subject)
	if name == "" {
		name = defaultBotDisplayName
	}
	return manifestFileStem + name + manifestFileExt
}

// manifestDisplayName is the name a manifest may actually carry, derived from
// whatever the caller named the app after.
//
// BOTH templates go through it, and neither may interpolate a caller's string
// raw. The names reaching them are AgentClass and AgentIdentity names, which
// this project validates as DNS-1123 subdomains — so a period is legal, and
// 253 characters are legal. Slack accepts neither: a dot in
// bot_user.display_name and a display_information.name past 35 characters are
// each refused as invalid_manifest, which appprovision/errors.go reads as
// unrecoverable, and which an operator pasting the document by hand sees as a
// rejection with no stated cause. An identity called
// "acme.slack-directory-for-the-platform-team-prod" is an ordinary name and
// hits both.
//
// The cut is visible rather than silent: the name is in the document the
// operator reads and pastes, so a shortened one is on the screen in front of
// them. Trimming AFTER the cut is what stops it ending on the '-' a fold left
// behind mid-word.
func manifestDisplayName(name string) string {
	folded := foldName(name)
	if len(folded) > manifestNameLimit {
		folded = strings.Trim(folded[:manifestNameLimit], "-")
	}
	if folded == "" {
		return defaultBotDisplayName
	}
	return folded
}

// SavedManifest is what became of the copy on disk: where it landed, whether
// it displaced a file that was already there, or why there is no file.
//
// A failed save is carried rather than returned because it must not end the
// run — the manifest is on the screen either way. It is reported instead, both
// on that screen and in the run summary, so it can never be merely dropped.
type SavedManifest struct {
	path     string
	replaced bool
	err      error
}

// SaveManifest writes the manifest beside the run, into dir (the working
// directory when empty).
//
// An existing file of the same name is overwritten: re-running the wizard for
// one agent regenerates the same manifest, and a directory slowly filling with
// numbered near-duplicates is worse than one file that is current. What it is
// NOT allowed to do is overwrite quietly, which is what `replaced` is for.
func SaveManifest(dir, subject, manifest string) SavedManifest {
	name := manifestFileName(subject)
	path := name
	display := "./" + name
	if dir != "" {
		path = filepath.Join(dir, name)
		display = path
	}

	// A Stat error other than "not there" is not evidence the file is absent,
	// so it is read as "something is there" — the honest direction for a claim
	// the user is being asked to trust.
	_, statErr := os.Stat(path)
	replaced := statErr == nil || !os.IsNotExist(statErr)

	if err := os.WriteFile(path, []byte(manifest), manifestFilePerm); err != nil {
		return SavedManifest{path: display, err: err}
	}
	return SavedManifest{path: display, replaced: replaced}
}

// MessageLines says what became of the copy on disk, wrapped to fit the note's
// column budget by keeping the path on a line of its own.
//
// A failed write points the operator back at THIS MESSAGE, because that is the
// only other place the manifest exists: the manual route hands it over as the
// error that ends the run (manualRouteRefusal), so there is no screen still
// holding it once they go looking.
func (s SavedManifest) MessageLines() string {
	if s.err != nil {
		return "This manifest is not saved to\n  " + s.path + "\n  (" + s.err.Error() + ")\nso copy it from this message before moving on."
	}
	if s.replaced {
		return "A copy is saved at\n  " + s.path + "\n  (replacing the file that was already there)."
	}
	return "A copy is saved at\n  " + s.path
}

// Path is where the copy landed, spelled the way the user is shown it. Empty
// only if nothing was ever attempted.
//
// Exported for a caller that names the file somewhere MessageLines' whole
// block does not fit — a one-line run summary, say — rather than for one that
// re-derives the sentence.
func (s SavedManifest) Path() string { return s.path }

// Err reports why the copy was not written, or nil. A caller deciding what to
// say about the file asks this; a caller SHOWING the outcome uses
// MessageLines, which already answers it in prose.
func (s SavedManifest) Err() error { return s.err }

// botScopesYAML renders the scopes the given feature set needs as the
// manifest's "oauth_config.scopes.bot" YAML list, one "      - <scope>" line
// per entry. Generated from Kind.FeatureSupport (features.go) via
// channelkinds.ScopesFor — rather than a literal list — so the manifest
// cannot request a scope the runtime does not need for the given feature
// set, or omit one it does.
func botScopesYAML(features []channelfeatures.Feature) string {
	var b strings.Builder
	for _, s := range channelkinds.ScopesFor(&Kind{}, features) {
		b.WriteString("      - ")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

// appManifestFor renders the Slack app manifest with displayName as both the
// app's display_information.name and the bot_user.display_name, requesting
// only the scopes the given features need. An empty displayName falls back
// to defaultBotDisplayName.
//
// It declares agent_view, which responds inline in messages and in-thread,
// rather than the thread-isolated assistant_view. **The flip is IRREVERSIBLE at
// the Slack API level** once a workspace adopts the new surface.
//
// Socket Mode is on, with event subscriptions for app_mention,
// message.{channels,groups,im}. The bot scope list is generated from
// Kind.FeatureSupport (features.go) via channelkinds.ScopesFor — see that file
// for what each scope covers and what breaks without it. Interactivity is
// enabled so message-action shortcuts (Restart from here) and view_submission
// callbacks reach the listener over the socket connection; the Home tab plus
// app_home_opened let the per-agent "Manage connections" button reach the
// identityd portal.
//
// member_joined_channel/member_left_channel are deliberately NOT subscribed
// here. They would feed a RelationshipSource's Invalidator
// (pkg/controllers/relationshipsource/invalidate.go) so it could wake a
// scoped reconcile between periodic syncs, but nothing wires that Invalidator
// up yet (no HTTP/NATS bridge constructs one
// or calls OnWake), so subscribing every per-agent app to these events today
// would only cost a busy workspace a permanent stream of unconsumed
// "events_api inner" INFO logs (listener.go's handleEventsAPI logs every
// inner event type it does not otherwise handle) for a feature operators
// cannot benefit from — and worse, the wizard note that used to advertise it
// asked for a manifest reinstall that bought nothing. Add them back WITH the
// consumer, not ahead of it: periodic sync (15m default) already finds a
// join/leave on its own, so this is a missing optimization, not a gap.
//
// assistant:write drives the native "thinking" indicator
// (assistant.threads.setStatus), and REQUIRES the workspace to have "Agents &
// AI Apps" enabled and the app's "Agent or Assistant" feature toggled on at
// https://api.slack.com/apps → your app → "Agent & Assistants".
//
// EVERY LINE HERE MUST STAY NARROW. The wizard shows this manifest on a huh
// note, which hard-wraps to the form's width — 61 columns on an 80-column
// terminal. A wrapped YAML line continues at column 0 inside an indented block,
// which Slack's manifest import REJECTS, and nothing in the render says so. The
// prose is ours, so the prose gives way;
// TestWizard_NoApp_ManifestFitsTheTTYBodyColumn holds a widening edit to it.
func appManifestFor(displayName string, features []channelfeatures.Feature) string {
	displayName = manifestDisplayName(displayName)
	return fmt.Sprintf(`display_information:
  name: %[1]s
features:
  bot_user:
    display_name: %[1]s
    always_online: true
  app_home:
    home_tab_enabled: true
    messages_tab_enabled: true
    messages_tab_read_only_enabled: false
  agent_view:
    agent_description: An agentprimitives-powered AI agent.
  shortcuts:
    - name: Restart from here
      type: message
      callback_id: ap_restart_from_here
      description: Edit and rerun from this message
oauth_config:
  scopes:
    bot:
%[2]ssettings:
  event_subscriptions:
    bot_events:
      - app_mention
      - message.channels
      - message.groups
      - message.im
      - app_home_opened
  interactivity:
    is_enabled: true
  socket_mode_enabled: true
`, displayName, botScopesYAML(features))
}

// BotTokenAppManifestFor renders the Slack app manifest for an app whose only
// product is a BOT TOKEN: no listener, no Socket Mode, no Home tab, no agent
// view, no event subscriptions — a bot user and the scopes the given feature
// set declares, and nothing else. `oap directory configure`'s credential-setup
// flow shows this so an operator creating a Slack app for a directory sync
// gets one that requests exactly what the sync calls.
//
// IT IS NOT appManifestFor WITH THE LISTENER PARTS TRIMMED, and the difference
// is not cosmetic. Slack validates a manifest's bot_events against the scopes
// the SAME manifest requests, and appManifestFor subscribes to
// message.channels, message.groups and message.im — which want the three
// history scopes. Render that template with a directory sync's feature set and
// the document names events whose scopes it does not ask for: Slack answers
// invalid_manifest, which appprovision/errors.go rightly reads as "our own
// generation is wrong and no fallback will help". The narrower document has no
// events to be inconsistent with, which is what
// TestBotTokenAppManifestSubscribesToNothing pins.
//
// The scope block is the SHARED half — botScopesYAML — so both manifests
// derive from Kind.FeatureSupport (features.go), and neither can request a
// scope its feature set does not declare or omit one it does.
//
// A feature set whose entries declare no scopes renders an empty bot list,
// which Slack refuses. The caller chooses the features; every caller today
// passes a set that declares some, and TestBotTokenAppManifestForDirectorySync
// holds that for the one this flow uses.
//
// always_online is FALSE here and true in appManifestFor, which is the one
// field that differs for a reason other than "no listener". It is what Slack
// shows beside the bot's name, and appManifestFor's app runs a Socket Mode
// connection that really is live, so claiming it is honest there. This app
// connects to nothing — a periodic sync calls the Web API and hangs up — so a
// green dot next to it would be a claim about a presence nobody keeps.
//
// EVERY LINE HERE MUST STAY NARROW, for the reason appManifestFor's own doc
// gives at length: this is shown on a huh note that hard-wraps at the form's
// width, and a wrapped YAML line is one Slack's manifest import rejects.
func BotTokenAppManifestFor(displayName string, features []channelfeatures.Feature) string {
	displayName = manifestDisplayName(displayName)
	return fmt.Sprintf(`display_information:
  name: %[1]s
features:
  bot_user:
    display_name: %[1]s
    always_online: false
oauth_config:
  scopes:
    bot:
%[2]s`, displayName, botScopesYAML(features))
}
