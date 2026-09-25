package passwordkind

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
)

// State keys this wizard answers. Stable: they are what a refusal names.
const (
	KeyPassword = "password"
	KeyConfirm  = "confirm"
)

// minPasswordLength is a floor, not a strength policy: this is a single-user
// local admin account with no external attack surface beyond the desktop's own
// /admin login, and the brute-force mitigation is the fixed per-attempt delay
// identityd adds, not password entropy.
const minPasswordLength = 8

// bcryptCost is the hashing cost used by this wizard and by HashPassword. 12 is a
// conservative modern default: comparison costs a few hundred milliseconds on
// desktop-class hardware, cheap enough for interactive login and expensive enough
// to blunt offline cracking of a leaked hash.
const bcryptCost = 12

// defaultSecretName and defaultSecretKey are the first-setup convention for
// where the password hash is stored. A re-setup honors whatever ref the
// existing CR declares instead; see secretRef.
const (
	defaultSecretName = "idp-password"
	defaultSecretKey  = "client_secret"
)

// HashPassword bcrypt-hashes pw at bcryptCost. Exported so a non-interactive
// setup path (the macOS desktop bundle's first-run provisioning) can hash a
// password without driving this package's screens.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(h), nil
}

// wizard is the `oap idp setup password` flow.
//
// As a screen sequence:
//
//	password  the new password, or blank to keep the stored one
//	confirm   the same password again, skipped when the stored one is kept
//
// There is no redirect URI and no access policy: nothing federates here, and
// the single local identity IS the account.
//
// in is carried from Screens to Result because the answers alone do not say which
// Secret to write: a re-setup must reuse the ref the existing CR declares, and
// whether a stored password exists is what makes a blank answer mean "keep it"
// rather than "you did not answer".
type wizard struct{ in idp.WizardInput }

func (w *wizard) Screens(_ context.Context, in idp.WizardInput) ([]tui.Screen, error) {
	w.in = in

	title := "New password"
	if in.SecretExists {
		title = "New password (leave blank to keep the stored one)"
	}

	return []tui.Screen{
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "password",
				Label:     "Password",
				Key:       KeyPassword,
				Title:     title,
				Guidance:  func(*tui.State) string { return passwordIntro },
				NoteLabel: "Password",
				// Never the value, and never a mask of it either: the project's
				// masker preserves the last four characters, which for a
				// password is four characters of the password.
				NoteValue:  idpscreens.SecretUpdated,
				NoteAbsent: idpscreens.SecretKept,
			},
			Optional: in.SecretExists,
			Check:    func(_ *tui.State, v string) error { return checkLength(v) },
		}),
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:    "confirm",
				Label: "Confirm",
				Key:   KeyConfirm,
				Title: "Confirm the password",
				// Nothing to confirm when the stored password is kept. Not merely
				// cosmetic: without this the run would demand a second copy of a
				// password the user deliberately did not retype.
				Skip: func(st *tui.State) bool {
					return st.Has(KeyPassword) && strings.TrimSpace(st.Get(KeyPassword)) == ""
				},
				// No summary line at all: the only thing this screen could record is
				// the password a second time.
			},
			Check: func(st *tui.State, v string) error {
				if v != strings.TrimSpace(st.Get(KeyPassword)) {
					return errors.New("the two passwords do not match")
				}
				return nil
			},
		}),
	}, nil
}

// passwordIntro is the lead-in above the first question. Every line is inside
// the note's column budget, which this package's tests assert.
const passwordIntro = "This is the password for signing in to the /admin\n" +
	"console. No external identity provider is involved —\n" +
	"the password is the only credential."

func (w *wizard) Result(st *tui.State) (idp.WizardOutput, error) {
	if st == nil {
		return idp.WizardOutput{}, errors.New("the setup questions were not answered")
	}

	identifier := defaultLocalIdentity
	if w.in.Existing != nil && w.in.Existing.ClientID != "" {
		identifier = w.in.Existing.ClientID
	}
	secretName, secretKey := secretRef(w.in)

	spec := spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:     "password",
		ClientID: identifier,
		// ClientSecretRef.Namespace is intentionally left empty here. The
		// oap idp setup dispatcher fills it with the operator namespace before
		// applying the CR.
		ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
			Name: secretName,
			Key:  secretKey,
		},
		// Password sign-in has no email/domain concept — the single local admin
		// identity IS the account. AllowAnyEmail=true satisfies the CR's fail-closed
		// "domains or explicit allow-any" webhook rule without a meaningless list.
		AllowAnyEmail: true,
	}
	res := idp.WizardOutput{Spec: spec, SecretName: secretName}

	// A blank password means "keep the stored one" only when there IS one. The
	// same blank on a first setup is an unanswered question, and hashing it
	// would leave an admin console whose password is the empty string.
	pw := strings.TrimSpace(st.Get(KeyPassword))
	if pw == "" {
		if !w.in.SecretExists {
			return idp.WizardOutput{}, errors.New("a password is required")
		}
		return res, nil
	}

	// Re-checked here rather than trusted to the fields above: huh's accessible
	// renderer has no error channel, so a run whose input ended mid-field arrives
	// with whatever was last bound and a nil error. This is the choke point; the
	// field validators only make a mistyped password recoverable rather than fatal.
	if err := checkLength(pw); err != nil {
		return idp.WizardOutput{}, err
	}
	if pw != strings.TrimSpace(st.Get(KeyConfirm)) {
		return idp.WizardOutput{}, errors.New("the two passwords do not match")
	}

	hash, err := HashPassword(pw)
	if err != nil {
		return idp.WizardOutput{}, err
	}
	res.SecretData = map[string][]byte{secretKey: []byte(hash)}
	return res, nil
}

// checkLength enforces the length floor. The message states the rule rather
// than the value, which is the one thing that must not be echoed back.
func checkLength(pw string) error {
	if len(pw) < minPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", minPasswordLength)
	}
	return nil
}

// secretRef is where the password hash lives. It defaults to the first-setup
// convention name, but on re-setup MUST honor whatever ref the existing CR
// declares — the same rule oidckind and googlekind follow — otherwise re-applying
// the CR points ClientSecretRef at a Secret that holds nothing.
func secretRef(in idp.WizardInput) (name, key string) {
	name, key = defaultSecretName, defaultSecretKey
	if in.Existing != nil && in.Existing.ClientSecretRef.Name != "" {
		name = in.Existing.ClientSecretRef.Name
		if in.Existing.ClientSecretRef.Key != "" {
			key = in.Existing.ClientSecretRef.Key
		}
	}
	return name, key
}
