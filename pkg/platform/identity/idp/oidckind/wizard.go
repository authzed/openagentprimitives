package oidckind

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
)

// KeyIssuer is the State key the issuer URL lands under. Stable: it is part of
// this wizard's public vocabulary, and what a refusal names.
const KeyIssuer = "issuer"

// defaultSecretName and defaultSecretKey are the first-setup convention for
// where the client secret is stored. A re-setup honors whatever ref the
// existing CR declares instead; see secretRef.
const (
	defaultSecretName = "idp-oidc"
	defaultSecretKey  = "client_secret"
)

// wizard is the `oap idp setup oidc` flow.
//
// As a screen sequence:
//
//	issuer         where the provider lives, plus the redirect URI to register
//	client-id      the client registered there
//	client-secret  its secret, or blank to keep the stored one
//	access         who may sign in
//	domains        which email domains, when access is restricted
//
// in is carried from Screens to Result because the answers alone do not say which
// Secret to write: a re-setup must reuse the ref the existing CR declares, and
// whether a stored secret exists is what makes a blank answer mean "keep it"
// rather than "you did not answer".
type wizard struct{ in idp.WizardInput }

func (w *wizard) Screens(_ context.Context, in idp.WizardInput) ([]tui.Screen, error) {
	w.in = in

	var defIssuer, defClientID string
	var defDomains []string
	var defAllowAny bool
	if in.Existing != nil {
		defIssuer = in.Existing.Issuer
		defClientID = in.Existing.ClientID
		defDomains = in.Existing.AllowedEmailDomains
		defAllowAny = in.Existing.AllowAnyEmail
	}

	return []tui.Screen{
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "issuer",
				Label:     "Issuer",
				Key:       KeyIssuer,
				Title:     "Issuer URL",
				Guidance:  func(*tui.State) string { return issuerIntro },
				Addresses: []tui.Address{idpscreens.CallbackAddress(in.CallbackURL)},
				NoteLabel: "Issuer",
			},
			Default: func() string { return defIssuer },
			Check:   func(_ *tui.State, v string) error { return validateIssuer(v) },
		}),
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "client-id",
				Label:     "Client ID",
				Key:       idpscreens.KeyClientID,
				Title:     "Client ID",
				NoteLabel: "Client ID",
			},
			Default: func() string { return defClientID },
		}),
		idpscreens.NewClientSecret(in.SecretExists),
		idpscreens.NewAccess(defAllowAny, defDomains),
		idpscreens.NewDomains(defDomains),
	}, nil
}

// issuerIntro is the lead-in above the first question. Every line is inside the
// note's column budget, which this package's tests assert.
const issuerIntro = "Register this cluster as an OAuth client with your\n" +
	"identity provider, adding the address below as an\nauthorized redirect URI."

func (w *wizard) Result(st *tui.State) (idp.WizardOutput, error) {
	if st == nil {
		return idp.WizardOutput{}, errors.New("the setup questions were not answered")
	}

	issuer := strings.TrimSpace(st.Get(KeyIssuer))
	if issuer == "" {
		return idp.WizardOutput{}, errors.New("an issuer URL is required")
	}
	if err := validateIssuer(issuer); err != nil {
		return idp.WizardOutput{}, err
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
			Kind:     "oidc",
			Issuer:   issuer,
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
// allow-any hands the cluster to anyone the provider will authenticate, and an
// empty domain list is refused by the CR's own webhook minutes later, as a
// validity failure with no obvious cause.
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

// validateIssuer accepts an https issuer, and http only for a loopback address
// — which is a local development provider, not something reachable from
// anywhere an interception would matter.
func validateIssuer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("that is not a URL: %w", err)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"):
		return nil
	default:
		return errors.New("the issuer URL must use https (http is allowed only for localhost)")
	}
}
