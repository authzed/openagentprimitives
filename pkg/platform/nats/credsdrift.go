package nats

import (
	"fmt"
	"strings"

	"github.com/nats-io/jwt/v2"
)

// CredsDrift reports why an already-stored decorated creds file no longer
// carries this grant, or "" when the two agree exactly.
//
// It exists because a minted user JWT is a FROZEN COPY of the grant that was
// in the binary when it was minted. Nothing on the bus re-reads a UserGrant at
// runtime, so a grant that gains a subject in a release only reaches an
// EXISTING cluster if something re-mints — and skipping that is invisible in
// the worst possible way: nats-server reports a permission violation
// ASYNCHRONOUSLY on the connection, so Publish returns nil and the caller
// answers its own client as though the message went out. Comparing what is
// stored against what is shipped is what turns "we changed the grant" into
// "the cluster has the grant".
//
// The answer is prose rather than a bool so the caller can say WHY it is
// rotating a credential (per the never-silently-drop rule): nothing else in
// the system would ever report the difference.
//
// Unparseable, unnamed or empty stored creds are drift, not an error: whatever
// is stored cannot be shown to carry the grant, and re-minting is both the
// correct repair and the only one available.
func (g UserGrant) CredsDrift(creds string) string {
	stored, err := GrantFromCreds(creds)
	if err != nil {
		return err.Error()
	}
	if stored.Name != g.Name {
		return fmt.Sprintf("the stored user JWT is named %q, not %q", stored.Name, g.Name)
	}
	if why := subjectSetDrift("publish", stored.PubAllow, g.PubAllow); why != "" {
		return why
	}
	return subjectSetDrift("subscribe", stored.SubAllow, g.SubAllow)
}

// GrantFromCreds reads back the grant a decorated creds file's user JWT
// actually carries — the inverse of MintUser, for the permissions half.
//
// It exists so "what does this cluster's stored credential permit?" is
// answerable without re-deriving JWT parsing at each call site, and so a test
// can put a STORED credential through the same server-side permission check a
// freshly-built grant goes through. The returned value is the claims as
// written: allow-lists in their stored order, no normalization.
func GrantFromCreds(creds string) (UserGrant, error) {
	if strings.TrimSpace(creds) == "" {
		return UserGrant{}, fmt.Errorf("no user JWT is stored")
	}
	token, err := jwt.ParseDecoratedJWT([]byte(creds))
	if err != nil {
		return UserGrant{}, fmt.Errorf("the stored creds file could not be parsed (%v)", err)
	}
	uc, err := jwt.DecodeUserClaims(token)
	if err != nil {
		return UserGrant{}, fmt.Errorf("the stored user JWT could not be decoded (%v)", err)
	}
	return UserGrant{Name: uc.Name, PubAllow: uc.Pub.Allow, SubAllow: uc.Sub.Allow}, nil
}

// subjectSetDrift compares one stored allow-list against the shipped one as a
// SET, and names the difference in both directions.
//
// Order is deliberately not significant: nats-server evaluates an allow-list
// as a set, so re-minting because two identical lists were written in a
// different order would rotate every principal's credential on every install
// for no gain. A REMOVED subject counts as drift too — leaving a withdrawn
// permission in a live JWT is the same staleness in the direction that
// actually matters for security.
func subjectSetDrift(direction string, stored, want []string) string {
	have := make(map[string]struct{}, len(stored))
	for _, s := range stored {
		have[s] = struct{}{}
	}
	need := make(map[string]struct{}, len(want))
	for _, s := range want {
		need[s] = struct{}{}
	}
	var missing, extra []string
	for _, s := range want {
		if _, ok := have[s]; !ok {
			missing = append(missing, s)
		}
	}
	for _, s := range stored {
		if _, ok := need[s]; !ok {
			extra = append(extra, s)
		}
	}
	switch {
	case len(missing) > 0 && len(extra) > 0:
		return fmt.Sprintf("the stored %s allow-list is missing %s and still carries withdrawn %s",
			direction, strings.Join(missing, ", "), strings.Join(extra, ", "))
	case len(missing) > 0:
		return fmt.Sprintf("the stored %s allow-list is missing %s", direction, strings.Join(missing, ", "))
	case len(extra) > 0:
		return fmt.Sprintf("the stored %s allow-list still carries withdrawn %s", direction, strings.Join(extra, ", "))
	}
	return ""
}
