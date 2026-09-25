package bronzethread

// Stand-in provider state: the ONE rule for a value that crossed in from
// outside our code.
//
// A meta tool's reply mixes two kinds of value. Most of it our own code
// composed — the surface's name, the refusal wording, the shape of the JSON —
// and every one of those is reproduced at replay by RUNNING that code. The rest
// came from a provider: an id GitHub minted, a commit a pull-request read
// resolved, a user id a workspace directory holds. A replay has no way to
// derive those, because they were never derivable.
//
// Three answers exist, and the order between them is not a preference:
//
//  1. A value WE mint gets an injectable minter, and the bundle records the
//     sequence. Already the answer for the operation and artifact families;
//     see mintedids.go.
//  2. A value a PROVIDER minted or holds gets the stand-in SEEDED from the
//     bundle, and our code composes the reply from it. Full fidelity of
//     everything we wrote; only the foreign value is pinned. That is this file.
//  3. Canning the reply outright is the LAST resort, and it is a real cost:
//     a canned reply is served whatever the code does, so a regression in the
//     very code the bundle exists to cover is handed the recorded answer and
//     passes. It is why a gate refusal is never canned (see the ToolErrors
//     ruling in steelthread's fold) and why nothing here cans a meta tool.
//
// Read (2) as the working primitive and (3) as an admission. A stand-in that
// answers from seeded state still runs the assembly that offered the tool, the
// tool's own argument parsing and refusals, the channel kind's composition, and
// — where there is one — the real HTTP round trip to the fixture provider. What
// is pinned is the number the provider chose, and nothing else.
//
// # The values must still be DERIVED, never invented
//
// Every field below is filled from a durable record, through the party that
// owns the format it is written in — a channel kind reads back the text it
// composed, never a reader guessing at it. A capture that could not derive a
// value leaves it empty and the self-check refuses; it does not make one up.
// Seeding a stand-in with a value nobody observed produces a bundle that
// replays cleanly and is evidence about nothing.

// StandIn is the provider-side state one bundle's replay must seed.
type StandIn struct {
	// TriggerStatusIDs are the identifiers the TRIGGER provider minted during
	// the recorded run, in mint order — for GitHub, the check run ids it
	// assigned.
	//
	// The stand-in provider hands these back as its own mints, so the id the
	// kind reports, patches by, and finds again is the id the recorded run saw.
	// That is what lets a step's own divergence check name every byte of the
	// reply rather than being narrowed around a number nobody could reproduce.
	//
	// ORDER is the contract, exactly as it is for MintedIDs: a run that opens
	// its statuses in a different order addresses a different object than the
	// recorded one did, and drawing from a fixed list in order is what makes
	// that visible.
	//
	// Empty leaves the stand-in minting its own, which is what every bundle
	// that never touches a trigger status wants.
	TriggerStatusIDs []string `json:"triggerStatusIDs,omitempty"`

	// Mentions is the channel user directory the `fake` kind serves.
	//
	// The fixture rewrite turns every non-trigger Channel into kind=fake, and
	// the real kind's directory goes with it — so a session that was OFFERED
	// lookup_user_for_mention is offered nothing at replay, and the recorded
	// tool catalog can no longer be reproduced. Seeding the fake kind's
	// directory is what puts the offer back.
	//
	// Nil advertises nothing, which is the default every existing bundle's tool
	// list stands on.
	Mentions *MentionStandIn `json:"mentions,omitempty"`
}

// MentionStandIn is what the fake kind advertises and answers as a directory.
type MentionStandIn struct {
	// Lookups are the MentionLookupKind values the stand-in advertises —
	// derived from what the RECORDED channel's own kind advertised, so the tool
	// is offered with the same input enum the run actually saw.
	//
	// Spelled as plain strings rather than the kind's own type: this package
	// owns a wire format and must not drag a channel-kind import into every
	// consumer of it. The driver converts on the way in.
	Lookups []string `json:"lookups,omitempty"`

	// Users are the directory entries a recorded run resolved.
	//
	// Empty is the ordinary case and is NOT a gap: a run that was offered the
	// tool and never called it has an offer to reproduce and no answer to. The
	// stand-in then refuses every lookup as not-found, which is the kind's own
	// sentinel and the honest answer for a directory holding nobody.
	Users []MentionUser `json:"users,omitempty"`
}

// MentionUser is one directory entry: what a run asked for, and what the real
// channel answered.
type MentionUser struct {
	// Kind is the MentionLookupKind the value was resolved under.
	Kind string `json:"kind"`
	// Value is the identifier that was looked up — an email, a display name.
	Value string `json:"value"`
	// ExternalID is the provider's own id for that user. THE foreign value:
	// nothing in our code could produce it.
	ExternalID string `json:"externalID"`
	// DisplayName is what the provider calls them. Carried because the kind's
	// LookupUser contract returns it, even though the tool's reply does not.
	DisplayName string `json:"displayName,omitempty"`
}

// FamilyTriggerStatus keys StandIn.TriggerStatusIDs into a MintedIDSequence, so
// a stand-in provider draws them with the same three properties the system's own
// minted ids get: exhaustion reports itself and names the index, leftovers are
// reported by Unused, and a different ORDER hands a recorded call an id that now
// addresses something else.
//
// Deliberately NOT a row in MintedIDFamilies. That table is what FamilyOf
// shape-matches a string against and what Bundle.validateMintedIDs admits into
// the mintedIDs map; a provider's identifiers have no shape we mint and no shape
// we could recognize — a check run id is a bare integer, indistinguishable from
// a line count. They are carried in standIn precisely because they cannot be
// found the way MintedIDs are found, and adding a row here would tell FamilyOf
// to start filing arbitrary numbers as mints.
const FamilyTriggerStatus = "triggerStatus"

// StandInMintedCredentialTypes are the credkind type names a REPLAY HARNESS
// stands a minter up for.
//
// A minted credential type reads nothing from the placeholder Secret a fixture
// emits beside it: every resolve calls an external minter through a collaborator
// that is nil wherever nobody configured one. So a captured session using one
// replays with its identity reporting healthy and every drawing call failing at
// dispatch — unless the harness serves that type against its own fixture
// provider.
//
// The list lives HERE, in the package that owns the bundle format, for the
// reason MintedIDFamilies does: two copies would drift. The capture reads it to
// decide whether to refuse; the replay driver is held to it by a test that
// fails when a named type has no stand-in wired. Neither can quietly disagree
// with the other about what is servable.
//
// Adding a name here is a promise the harness keeps, not a wish. A type listed
// with nothing behind it turns a loud capture refusal into a silent replay
// failure — strictly worse than not listing it.
var StandInMintedCredentialTypes = []string{
	// GitHub App installation tokens, minted against the same fixture provider
	// the trigger-status surface is pointed at.
	"githubApp",
}
