package channelkinds

import (
	"context"
	"errors"

	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
)

// ErrProfileNotFound is returned when the channel has no profile for the
// given user. Distinct from a transport error so callers can degrade quietly
// (no profile block) instead of logging an alarm for an ordinary miss.
var ErrProfileNotFound = errors.New("channelkinds: no profile for user")

// UserProfileProvider is implemented by channel kinds that can supply profile
// detail for a user. Optional and discovered by type assertion, so a kind that
// cannot serve profiles needs no code at all.
//
// Implementations are pure transport: they return kind-native data and never
// consult SpiceDB, read the AgentClass, or apply the operator's field
// allowlist. Allowlisting is userprofile.Filter's job, in exactly one place —
// a kind that pre-filtered would become a second, drifting gate.
type UserProfileProvider interface {
	// ProfileFields returns the fields this kind can populate. MUST NOT
	// perform I/O — it is read at assembly time to decide whether offering
	// the capability is meaningful at all.
	ProfileFields() []userprofile.Field

	// FetchProfileByEmail resolves one user's profile by their verified
	// email. Returns ErrProfileNotFound when the channel has no such user.
	//
	// Email is the key (rather than a channel-native id) because it is what
	// the runner can derive from a turn's canonical Author subject without a
	// cluster round-trip. A user with no verified email — a guest, a
	// foreign-workspace member — has no resolvable profile here, which is the
	// intended fail-closed behavior: their self-set profile text is exactly
	// the text least worth trusting.
	FetchProfileByEmail(ctx context.Context, deps LookupDeps, email string) (userprofile.Profile, error)
}
