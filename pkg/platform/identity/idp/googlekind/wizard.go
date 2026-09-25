package googlekind

import (
	"context"
	"errors"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
)

// clientIDSuffix is what a Google OAuth web-application client ID ends with.
// A value without it is almost always the wrong field copied out of the
// console, but it is not refused: Google has changed this shape before, and a
// wizard that refuses a legitimate credential is worse than one that flags it.
const clientIDSuffix = ".apps.googleusercontent.com"

// defaultSecretName and defaultSecretKey are the first-setup convention for
// where the client secret is stored. A re-setup honors whatever ref the
// existing CR declares instead; see secretRef.
const (
	defaultSecretName = "idp-google"
	defaultSecretKey  = "client_secret"
)

// wizard is the `oap idp setup google` flow.
//
// As a screen sequence:
//
//	client-id      the web-application client registered in the console
//	client-secret  its secret, or blank to keep the stored one
//	access         who may sign in
//	domains        which email domains, when access is restricted
//
// There is no issuer question: the kind pins Google's own issuer.
//
// in is carried from Screens to Result because the answers alone do not say which
// Secret to write: a re-setup must reuse the ref the existing CR declares, and
// whether a stored secret exists is what makes a blank answer mean "keep it"
// rather than "you did not answer".
type wizard struct{ in idp.WizardInput }

func (w *wizard) Screens(_ context.Context, in idp.WizardInput) ([]tui.Screen, error) {
	w.in = in

	var defClientID string
	var defDomains []string
	var defAllowAny bool
	if in.Existing != nil {
		defClientID = in.Existing.ClientID
		defDomains = in.Existing.AllowedEmailDomains
		defAllowAny = in.Existing.AllowAnyEmail
	}

	return []tui.Screen{
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "client-id",
				Label:     "Client ID",
				Key:       idpscreens.KeyClientID,
				Title:     "Client ID",
				Guidance:  func(*tui.State) string { return consoleIntro },
				Addresses: []tui.Address{idpscreens.CallbackAddress(in.CallbackURL)},
				NoteLabel: "Client ID",
				// The shape check lives in the summary rather than in a validator
				// because it is a suspicion, not a rule: refusing here would block a
				// legitimate client ID whose shape Google changed, while a summary line
				// survives the run and is read when sign-in does not work.
				NoteValue: describeClientID,
			},
			Default: func() string { return defClientID },
		}),
		idpscreens.NewClientSecret(in.SecretExists),
		idpscreens.NewAccess(defAllowAny, defDomains),
		idpscreens.NewDomains(defDomains),
	}, nil
}

// consoleIntro is the lead-in above the first question. Every line is inside
// the note's column budget, which this package's tests assert.
const consoleIntro = "In the Google Cloud console, under APIs & Services >\n" +
	"Credentials, create an OAuth client ID of type\n" +
	"\"Web application\" and add the address below to its\n" +
	"authorized redirect URIs. Copy the client ID it shows;\n" +
	"it ends with " + clientIDSuffix + "."

// describeClientID is the summary line for the collected client ID: the value,
// plus a flag when it does not look like one Google issued.
func describeClientID(v string) string {
	if v != "" && !strings.HasSuffix(v, clientIDSuffix) {
		return v + " (does not end with " + clientIDSuffix + " — check you copied the client ID)"
	}
	return v
}

func (w *wizard) Result(st *tui.State) (idp.WizardOutput, error) {
	if st == nil {
		return idp.WizardOutput{}, errors.New("the setup questions were not answered")
	}

	clientID := strings.TrimSpace(st.Get(idpscreens.KeyClientID))
	if clientID == "" {
		return idp.WizardOutput{}, errors.New("a client ID is required")
	}

	// A blank secret means "keep the stored one" only when there IS one. The
	// same blank on a first setup is an unanswered question, and storing it
	// would leave the cluster authenticating with an empty client secret.
	clientSecret := strings.TrimSpace(st.Get(idpscreens.KeyClientSecret))
	if clientSecret == "" && !w.in.SecretExists {
		return idp.WizardOutput{}, errors.New("a client secret is required")
	}

	allowedDomains, allowAny, err := resolveAccess(st)
	if err != nil {
		return idp.WizardOutput{}, err
	}

	secretName, secretKey := secretRef(w.in)
	res := idp.WizardOutput{
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     "google",
			ClientID: clientID,
			// ClientSecretRef.Namespace is intentionally left empty here. The
			// oap idp setup dispatcher fills it with the operator namespace
			// before applying the CR.
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Name: secretName,
				Key:  secretKey,
			},
			AllowedEmailDomains: allowedDomains,
			AllowAnyEmail:       allowAny,
		},
		SecretName: secretName,
	}
	if clientSecret != "" {
		res.SecretData = map[string][]byte{secretKey: []byte(clientSecret)}
	}
	return res, nil
}

// resolveAccess reads the sign-in policy the run chose. It refuses an unanswered
// access question rather than defaulting, because both defaults are wrong:
// allow-any hands the cluster to any Google account at all, and an empty domain
// list is refused by the CR's own webhook minutes later, as a validity failure
// with no obvious cause.
func resolveAccess(st *tui.State) (domains []string, allowAny bool, err error) {
	switch st.Get(idpscreens.KeyAccess) {
	case idpscreens.AccessAny:
		return nil, true, nil
	case idpscreens.AccessDomains:
		domains = idpscreens.ParseDomains(st.Get(idpscreens.KeyDomains))
		if len(domains) == 0 {
			return nil, false, errors.New("no allowed email domains were supplied")
		}
		return domains, false, nil
	default:
		return nil, false, errors.New("it was not decided who may sign in")
	}
}

// secretRef is where the client secret lives. It defaults to the first-setup
// convention name, but on re-setup MUST honor whatever ref the existing CR
// declares (a hand-authored or pre-declared secretRef with a non-default name or
// key) — otherwise re-applying the CR points ClientSecretRef at a Secret that
// holds nothing.
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
