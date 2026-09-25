// Package nats provides NATS decentralized-JWT identity generation, per-client
// user-JWT minting, TLS material, and a connection helper. The agentprimitives
// control plane runs over an authenticated, TLS NATS bus; this package is the
// single source of truth for the credential shapes.
package nats

import (
	"fmt"
	"strings"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// Identity is the durable NATS trust material generated once per install.
// The seeds are secret; the JWTs and public keys are not.
type Identity struct {
	OperatorSeed []byte // operator NKey seed — kept for completeness, not used at runtime
	OperatorJWT  string // self-signed operator JWT — goes in the server config

	AccountPublicKey string // account identity public key
	AccountJWT       string // account JWT, signed by the operator — goes in the server resolver

	AccountSigningSeed      []byte // account signing-key seed — the operator mints user JWTs with this
	AccountSigningPublicKey string // account signing-key public key
}

// GenerateIdentity creates a fresh operator + account + account signing key.
// Called once by `oap install`; idempotency is the caller's concern.
func GenerateIdentity() (*Identity, error) {
	opKP, err := nkeys.CreateOperator()
	if err != nil {
		return nil, fmt.Errorf("create operator nkey: %w", err)
	}
	opPub, err := opKP.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("operator public key: %w", err)
	}
	opSeed, _ := opKP.Seed()

	accKP, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("create account nkey: %w", err)
	}
	accPub, err := accKP.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("account public key: %w", err)
	}

	signKP, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("create account signing nkey: %w", err)
	}
	signPub, err := signKP.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("account signing public key: %w", err)
	}
	signSeed, _ := signKP.Seed()

	opClaims := jwt.NewOperatorClaims(opPub)
	opClaims.Name = "agentprimitives"
	opJWT, err := opClaims.Encode(opKP)
	if err != nil {
		return nil, fmt.Errorf("encode operator jwt: %w", err)
	}

	accClaims := jwt.NewAccountClaims(accPub)
	accClaims.Name = "AP"
	accClaims.SigningKeys.Add(signPub)
	accJWT, err := accClaims.Encode(opKP)
	if err != nil {
		return nil, fmt.Errorf("encode account jwt: %w", err)
	}

	return &Identity{
		OperatorSeed:            opSeed,
		OperatorJWT:             opJWT,
		AccountPublicKey:        accPub,
		AccountJWT:              accJWT,
		AccountSigningSeed:      signSeed,
		AccountSigningPublicKey: signPub,
	}, nil
}

// UserGrant describes the subject permissions for one minted user.
type UserGrant struct {
	// Name identifies the principal. It appears in the user JWT, and it is
	// also the single input from which BOTH sides of the request/reply inbox
	// are derived — see InboxPrefixFor. Required; MintUser rejects an empty
	// Name rather than mint a user whose inbox cannot be scoped.
	Name string
	// PubAllow is the publish allow-list. An empty list is NOT deny-all: a
	// nats-server builds a publish permission only when the allow or deny list
	// is non-empty, so an empty list leaves the user able to publish ANYWHERE.
	// The comment here read "empty means deny-all publish" and was wrong in the
	// direction that matters — the over-grant it hid is recorded at
	// webdNATSGrant in cmd/oap/internal/installcmd/install.go. Every grant that must not publish
	// needs an explicit narrow list, never an omitted field.
	PubAllow []string
	// SubAllow is the subscribe allow-list, with the same semantics: an empty
	// list is allow-all, not deny-all.
	SubAllow []string
}

// InboxRoot is the top-level subject every principal's request/reply inbox
// lives under. Keeping the per-principal prefixes under the nats.go default
// root is deliberate: a RESPONDER must publish into whichever principal asked
// it, and a responder grant of "_INBOX.>" therefore keeps covering every
// narrowed inbox without knowing any requester's name.
const InboxRoot = "_INBOX"

// InboxPrefixFor returns the request/reply inbox prefix for the principal whose
// minted user JWT is named name — "_INBOX.<name>".
//
// It is the ONLY definition of that string, and both sides of the wire go
// through it: the minted SubAllow entry (via InboxSubjectFor) and the client's
// nats.CustomInboxPrefix (via buildOptions). A grant and a client that disagree
// about the inbox do not leak — they break EVERY request/reply on the bus,
// silently and totally, because the requester subscribes to one subject and the
// server authorizes another. One function over one input makes that
// unrepresentable.
//
// Why per principal: on a shared "_INBOX." root with every grant allowing
// "_INBOX.>", any principal could subscribe to the root and read every reply on
// the bus — including another session's channel thread history, which
// pkg/channels/channelsd/historyresp keeps per session by deriving the channel
// key from the request SUBJECT. The reply travels on the inbox, not that
// subject, so scoping the request without scoping the inbox moves the read one
// hop sideways rather than closing it.
//
// Characters outside [A-Za-z0-9_-] become '-' so the prefix is always exactly
// two tokens: a '.' would silently add one, and nats.CustomInboxPrefix rejects
// '*' and '>' outright. That map is LOSSY, so this function is not injective
// and cannot be — it must stay total over any name handed to it. Distinctness
// is the caller's to establish: a name composed from more than one identifier
// must be built with PrincipalName, which refuses inputs this map would fold
// together.
func InboxPrefixFor(name string) string {
	return InboxRoot + "." + sanitizeInboxToken(name)
}

// InboxSubjectFor returns the SubAllow entry covering the principal's inbox:
// "_INBOX.<name>.>". nats.go builds its reply subjects as
// "<prefix>.<nuid>.<token>", so the trailing '>' is required — a '*' would
// match only one of the two.
func InboxSubjectFor(name string) string {
	return InboxPrefixFor(name) + ".>"
}

func sanitizeInboxToken(name string) string {
	return strings.Map(func(r rune) rune {
		if inboxTokenFixedPoint(r) {
			return r
		}
		return '-'
	}, name)
}

// inboxTokenFixedPoint reports whether sanitizeInboxToken leaves r alone. Every
// other rune becomes '-', which is what makes that map lossy — and lossy is
// what PrincipalName has to defend against.
func inboxTokenFixedPoint(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		return true
	default:
		return false
	}
}

// PrincipalNameDelimiter joins the parts of a composite principal name.
//
// '_' is the only character that can do this job, and both halves are
// load-bearing:
//
//   - Nothing can CONTAIN it. Kubernetes namespaces and object names are
//     DNS-1123, which admits [a-z0-9-] plus '.' for names, and never '_'.
//   - Nothing can PRODUCE it. sanitizeInboxToken emits only '-', so no part can
//     be folded into a delimiter on the way to the subject token.
//
// '-' and '.' both fail the first test, being legal inside the identifiers
// being joined: a "runner-<ns>-<name>" scheme maps (team-a, bot) and
// (team, a-bot) onto one inbox.
const PrincipalNameDelimiter = "_"

// PrincipalName joins parts into a UserGrant.Name that is injective over those
// parts, or returns an error naming the part that would break it.
//
// It can FAIL rather than being a concatenation because the name is the single
// input to InboxPrefixFor, and thus to both sides of the request/reply inbox.
// Two principals producing one name share one inbox root, and each one's minted
// JWT then genuinely authorizes reading the other's replies — silently and
// correct-looking on the wire, since both sides derive the same string and
// nothing disagrees. Construction is the only place it can be caught, so an
// unsafe combination is refused rather than sanitized (AGENTS.md: make the
// unsafe combination unrepresentable).
//
// Each part must be non-empty and drawn from [A-Za-z0-9-] — precisely the fixed
// points of sanitizeInboxToken, minus the delimiter:
//
//   - Excluding the delimiter keeps the split unambiguous, so distinct part
//     lists cannot join to one string.
//   - Excluding everything sanitizeInboxToken rewrites keeps the join injective
//     THROUGH that map. '.' is the live case: legal in a DNS-1123 subdomain and
//     folded to '-', so ("ns", "a.b") and ("ns", "a-b") collide under ANY
//     delimiter. Only refusing the input fixes a lossy transform downstream.
//
// Sanitization is therefore the identity on the returned name, and splitting it
// on the delimiter recovers the parts exactly.
func PrincipalName(parts ...string) (string, error) {
	if len(parts) == 0 {
		return "", fmt.Errorf("nats: principal name requires at least one part")
	}
	for i, p := range parts {
		if p == "" {
			return "", fmt.Errorf("nats: principal name part %d is empty; an unset identifier must not be joined into a principal name", i)
		}
		for _, r := range p {
			// Allowed == a fixed point of sanitizeInboxToken that is not the
			// delimiter, i.e. exactly [A-Za-z0-9-].
			if inboxTokenFixedPoint(r) && !strings.ContainsRune(PrincipalNameDelimiter, r) {
				continue
			}
			return "", fmt.Errorf(
				"nats: principal name part %d (%q) contains %q; parts must match [A-Za-z0-9-] so the %q-joined name stays injective (%q is the delimiter, and every other rune folds to '-' in the inbox subject token)",
				i, p, string(r), PrincipalNameDelimiter, PrincipalNameDelimiter)
		}
	}
	return strings.Join(parts, PrincipalNameDelimiter), nil
}

// MintUser mints a NATS user JWT for the grant, signed by the account signing
// key, and returns a decorated NATS creds file (JWT + user seed) ready to be
// written to disk and passed to nats.UserCredentials.
func MintUser(id *Identity, g UserGrant) (string, error) {
	// Fail closed on an unnamed grant. The name is what scopes the principal's
	// reply inbox on both sides (InboxPrefixFor); minting without one would
	// hand out a user whose inbox cannot be narrowed and whose creds file
	// carries nothing for buildOptions to derive a prefix from.
	if strings.TrimSpace(g.Name) == "" {
		return "", fmt.Errorf("nats: UserGrant.Name is required (it scopes the principal's reply inbox)")
	}
	signKP, err := nkeys.FromSeed(id.AccountSigningSeed)
	if err != nil {
		return "", fmt.Errorf("load account signing seed: %w", err)
	}
	userKP, err := nkeys.CreateUser()
	if err != nil {
		return "", fmt.Errorf("create user nkey: %w", err)
	}
	userPub, err := userKP.PublicKey()
	if err != nil {
		return "", fmt.Errorf("user public key: %w", err)
	}
	userSeed, _ := userKP.Seed()

	uc := jwt.NewUserClaims(userPub)
	uc.Name = g.Name
	uc.IssuerAccount = id.AccountPublicKey
	uc.Pub.Allow = g.PubAllow
	uc.Sub.Allow = g.SubAllow
	userJWT, err := uc.Encode(signKP)
	if err != nil {
		return "", fmt.Errorf("encode user jwt: %w", err)
	}

	creds, err := jwt.FormatUserConfig(userJWT, userSeed)
	if err != nil {
		return "", fmt.Errorf("format creds: %w", err)
	}
	return string(creds), nil
}
