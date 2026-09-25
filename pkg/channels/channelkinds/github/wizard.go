// The manifests, the shared validators and the operator-facing text for
// `oap channel create --kind github`. What the flow ASKS — Inputs, the
// browser handoff, Resolve and Result — is in wizard_data.go; this file is
// what those call.
//
// Result emits the Secret + Channel manifests for the CLI dispatcher to
// apply, from the answer map alone.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github/appprovision"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
)

// The answer keys this wizard declares or derives. Stable strings: a caller
// seeding answers from flags for a non-interactive run addresses the
// questions by these.
const (
	// keyAgentClass is the shared binding key, aliased so this package's own
	// reads stay short. The STRING is wizardkeys' — see KeyChannelName's doc
	// for why a kind must never retype one of these.
	keyAgentClass = wizardkeys.KeyAgentClass

	// keyOrg holds the App's OWNER — an organization login or a personal
	// account's login, whichever keyOwnerType named. The key's STRING stays
	// "org": it is the spelling `--answer org=…` has always used, and renaming
	// it would refuse a scripted run that has been correct all along, for no
	// gain a prompt cannot deliver.
	keyOrg = "org"

	// keyOwnerType is which KIND of account keyOrg names, because GitHub's
	// App-creation and installation pages live on different paths for the two
	// and nothing about the login itself distinguishes them.
	keyOwnerType = "owner-type"

	// keyAppSource is WHERE THE APP COMES FROM: this run registers it, or the
	// operator already has one and is handing over its details.
	//
	// It is a question about AUTHORITY as much as about which prompts follow.
	// An App this run registered may later have its webhook URL repointed by
	// the channel controller — that is what keyProvisionedByOAP authorizes —
	// and an App the operator brought may not, because it is someone else's
	// resource and a wrong webhook URL on it is a drift finding rather than
	// something to correct. The two routes therefore differ in what the
	// produced Channel is ALLOWED to cause, not only in what it was asked.
	keyAppSource = "app-source"

	// The two values keyAppSource takes, ANSWER VALUES rather than display
	// text for the reason ownerTypeOrg and ownerTypeUser are: a script pins
	// `--answer app-source=existing`, while what an operator reads is the
	// EnumLabels beside them (wizard_data.go).
	appSourceCreate   = "create"
	appSourceExisting = "existing"

	// The two values keyOwnerType takes. They are ANSWER VALUES, not display
	// text — `--answer owner-type=user` is a spelling a script pins — so what
	// an operator reads is the EnumLabels beside them (wizard_data.go).
	ownerTypeOrg  = "organization"
	ownerTypeUser = "user"

	// keyExternalBaseURL is the shared "where is this cluster reachable from
	// outside?" key, aliased so this package's own reads stay short. The
	// STRING is wizardkeys' — it moved there because the .oap install planner
	// pre-seeds it, and reaching into this package for it would have made the
	// planner branch on the kind.
	keyExternalBaseURL = wizardkeys.KeyExternalBaseURL

	// The four keys below double as the Channel's required Secret keys
	// (Kind{}.RequiredSecretKeys) — see secretData, which reads them by
	// exactly these names rather than maintaining a second mapping.
	keyAppID          = "app-id"
	keySlug           = "slug"
	keyPrivateKeyPEM  = "private-key"
	keyWebhookSecret  = "webhook-secret"
	keyInstallationID = "installation-id"

	// keyPrivateKeyPath is NOT a required answer for Result — it is the manual
	// fallback route's own intermediate key, holding the path to a downloaded
	// .pem file. Resolve reads the file and derives keyPrivateKeyPEM from it,
	// which is what Result actually reads. See readPrivateKeyPEM's doc for
	// why a path is asked for rather than a pasted multi-line key.
	keyPrivateKeyPath = "private-key-path"

	// keyProvisionedByOAP is set by the handoff's Complete, and ONLY there:
	// its presence means this run POSTed the manifest that created the App,
	// as distinct from having been handed an App's credentials. Result reads
	// it to decide whether to stamp channelkinds.AnnotationAppProvisionedBy.
	//
	// It is DELIBERATELY NOT A DECLARED QUESTION. The marker is what later
	// authorizes repointing that App's webhook — an outward-facing write —
	// and a client builds its accepted --answer set from the questions a kind
	// declares (see checkAnswerKeys in cmd/oap/internal/channelwizard). Left
	// undeclared, no flag can claim this tool provisioned an App it did not;
	// declared, `--answer` would hand that claim to any caller. A test pins
	// the property rather than trusting it to stay true.
	keyProvisionedByOAP = "provisioned-by-oap"

	credsSecretNameSuffix = "-creds"
)

// converter is the subset of *appprovision.HTTPClient the App-creation step
// needs — an interface so a test can substitute a fake without a network.
// Used by the handoff's Complete (wizard_data.go).
type converter interface {
	Convert(ctx context.Context, code string) (*appprovision.Conversion, error)
}

// wizard is this kind's channelkinds.Wizard. Kind.Wizard returns a fresh
// one per call.
//
// convert is an injected collaborator, wired once and never written by a run:
// the handoff's Complete closes over it, and holding it is what "does I/O"
// means. Nothing else lives here, and nothing may — Result must be callable
// on a value no Inputs call ever touched, so per-run state on the receiver is
// exactly what channelkinds.Wizard.Result rules out.
type wizard struct {
	convert converter
}

// requiredGithubAnswers reads and validates the ten answers Result needs,
// trimmed, failing closed on the first missing one rather than emitting a
// Secret with an empty key or a Channel bound to no agent.
//
// get reads one answer by key rather than taking the map, so a caller with
// answers in another shape can still be held to the same check.
//
// The SHAPE checks are re-run here and not only where the answers were asked
// for: a channel wizard's questions carry no validation a client evaluates —
// see channelkinds.ValidateInputs on why a Validation would be silently
// ignored — so this is the ONLY check there is on the route that never touches
// the handoff. An App ID that is not a number produces a Secret the githubApp
// credkind cannot mint a token from, which surfaces as a Channel that never
// connects; a malformed owner login reaches the operator as three post-setup
// links to pages that do not exist, and a wrong owner TYPE reaches them as
// links to the wrong half of github.com.
//
// The owner and its type are checked HERE as well as in manifestParams because
// the two routes are disjoint. A run whose flags already carry the App's
// credentials has no handoff at all (see Handoff), so manifestParams is never
// called and nothing else would look at either before they are written into
// the Channel's own guidance.
func requiredGithubAnswers(get func(key string) string) (map[string]string, error) {
	answers := make(map[string]string, 10)
	for _, key := range []string{
		keyAgentClass, keyOwnerType, keyOrg, wizardkeys.KeyChannelName, wizardkeys.KeyAuthzSubject,
		keyAppID, keySlug, keyPrivateKeyPEM, keyWebhookSecret, keyInstallationID,
	} {
		v := strings.TrimSpace(get(key))
		if v == "" {
			return nil, fmt.Errorf("github wizard: %q was not answered", key)
		}
		answers[key] = v
	}
	if err := validateAppID(answers[keyAppID]); err != nil {
		return nil, err
	}
	if err := validateInstallationID(answers[keyInstallationID]); err != nil {
		return nil, err
	}
	// The owner type is checked before anything derived from it, for
	// ownerIsUser's reason: an unrecognized value would be read as "an
	// organization" and quietly send a personal account to an address that
	// 404s.
	if err := validateOwnerType(answers[keyOwnerType]); err != nil {
		return nil, fmt.Errorf("github wizard: %s: %w", keyOwnerType, err)
	}
	if err := validateOwnerLogin(answers[keyOrg]); err != nil {
		return nil, fmt.Errorf("github wizard: %s: %w", keyOrg, err)
	}
	// Shape, not just presence, and for the same reason the two IDs above are
	// re-checked here: nothing else on this side evaluates it, and a malformed
	// "service:…" takes the bound AgentClass to Valid=False.
	if err := wizardkeys.ValidateAuthzSubject(answers[wizardkeys.KeyAuthzSubject]); err != nil {
		return nil, fmt.Errorf("github wizard: %w", err)
	}
	return answers, nil
}

// githubOutput builds the Secret + Channel manifests and next-steps notes from
// the ten required answers. Summary is left to Result, which is where the
// run's decisions are stated — see channelkinds.WizardOutput.Summary.
//
// provisionedHere is passed rather than read out of answers because it is not
// one of the ten: it is the provenance Result stamps the Channel with, and the
// notes below have to say the same thing the annotation does or the operator
// is told one story and the controller acts on another.
func githubOutput(namespace string, answers map[string]string, provisionedHere bool) (channelkinds.WizardOutput, error) {
	channelName := answers[wizardkeys.KeyChannelName]
	secretName := credsSecretName(channelName)

	data, err := secretData(answers)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName, Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	channel := &spiceboxv1alpha1.Channel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "agentprimitives.authzed.com/v1alpha1",
			Kind:       "Channel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: channelName, Namespace: namespace,
		},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:         "github",
			Role:         spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:   answers[keyAgentClass],
			AuthzSubject: answers[wizardkeys.KeyAuthzSubject],
			SessionScope: "auto",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{
				SecretName: secretName,
			},
			GitHub: &spiceboxv1alpha1.GitHubChannelConfig{
				AppSlug: answers[keySlug],
			},
		},
	}

	return channelkinds.WizardOutput{
		SecretManifest:  secret,
		ChannelManifest: channel,
		Notes: postSetupNotes(postSetupFacts{
			channelName:     channelName,
			agentClass:      answers[keyAgentClass],
			namespace:       namespace,
			owner:           answers[keyOrg],
			ownerType:       answers[keyOwnerType],
			slug:            answers[keySlug],
			appID:           answers[keyAppID],
			provisionedHere: provisionedHere,
		}),
	}, nil
}

// secretData builds the Secret's Data from the answered credentials, keyed
// by exactly the strings Kind{}.RequiredSecretKeys(nil) returns — read from
// there rather than re-listing the four names a second time, so a change to
// the required set cannot silently leave this map stale.
func secretData(answers map[string]string) (map[string][]byte, error) {
	required := Kind{}.RequiredSecretKeys(nil)
	data := make(map[string][]byte, len(required))
	for _, k := range required {
		v, ok := answers[k]
		if !ok {
			// Reached only if RequiredSecretKeys and this wizard's answered-key
			// set (both listed in Result, both spelled identically) ever drift —
			// a build-time coupling, not a runtime one, but fail loudly rather
			// than emit a Secret missing a key the Channel controller requires.
			return nil, fmt.Errorf("github wizard: no answer for required secret key %q", k)
		}
		data[k] = []byte(v)
	}
	return data, nil
}

func credsSecretName(channelName string) string { return channelName + credsSecretNameSuffix }

// postSetupFacts is what the notes below are rendered from. A struct rather
// than seven positional strings: they are all strings, several are adjacent
// names of similar things (the owner, the slug, the Channel), and a transposed
// pair would compile and print a sentence that is quietly wrong.
type postSetupFacts struct {
	channelName string
	agentClass  string
	namespace   string
	owner       string
	ownerType   string
	slug        string
	appID       string
	// provisionedHere is whether THIS run registered the App, which is the
	// same fact Result stamps the Channel with
	// (channelkinds.AnnotationAppProvisionedBy) and therefore the same fact
	// that decides whether this cluster may ever write to the App upstream.
	provisionedHere bool
}

// postSetupNotes is the guidance the CLI prints after applying — the facts
// about the App that now exists, which belong after the run rather than as a
// wall of prose in front of the first question.
//
// AN APP THIS RUN DID NOT REGISTER GETS ONE EXTRA LINE, and it is the only
// place the operator learns this after the run: the guidance that said it was
// on a question screen the alt-screen has since released, whereas these land
// in plain scrollback. What it states is a real behavioural difference they
// will otherwise meet as a surprise — a Channel reporting webhook-URL drift
// that never clears, because clearing it would mean editing an App that is not
// this cluster's to edit.
func postSetupNotes(f postSetupFacts) []string {
	notes := []string{
		fmt.Sprintf("Created Channel resource %q (a Kubernetes object) bound to AgentClass %q in namespace %q.", f.channelName, f.agentClass, f.namespace),
		fmt.Sprintf("GitHub App %q (id %s) is registered under %s. Its private key and webhook secret live only in this Channel's credentials Secret — GitHub does not show either again.", f.slug, f.appID, f.owner),
	}
	if !f.provisionedHere {
		notes = append(notes, fmt.Sprintf(
			"This run did not register that App, so it never changes its settings. If its webhook URL stops matching %s, this Channel reports the drift and leaves the App alone.",
			f.channelName))
	}
	return append(notes, fmt.Sprintf(
		"Confirm the App's installation grants access to every repository this Channel should review: %s",
		installationsURL(f.ownerType, f.owner)))
}

// appDisplayName derives the App's requested display name from the owner and
// Channel name. GitHub App names are unique across all of github.com (see
// appprovision.Params.Name's doc), so this is a starting suggestion, not a
// guarantee — a collision surfaces as a failed handoff, which falls through
// to the manual fallback questions the same as any other App-creation error.
func appDisplayName(owner, channelName string) string { return owner + "-" + channelName }

// ---------------------------------------------------------------------------
// basics: org + external base URL
// ---------------------------------------------------------------------------

// ownerLoginPattern is GitHub's own login-format rule: alphanumeric or single
// hyphens, never leading/trailing, at most 39 characters. ONE pattern for both
// owner kinds because GitHub applies one rule — an organization login and a
// personal account's login are drawn from the same namespace, which is why an
// answer alone cannot say which of the two it is (see keyOwnerType).
var ownerLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

func validateOwnerLogin(in string) error {
	owner := strings.TrimSpace(in)
	if owner == "" {
		return errors.New("a GitHub organization or user login is required")
	}
	if !ownerLoginPattern.MatchString(owner) {
		return fmt.Errorf("%q does not look like a GitHub organization or user login (letters, digits, single hyphens, max 39 chars)", owner)
	}
	return nil
}

// validateOwnerType refuses anything but the two declared values.
//
// FAIL-CLOSED IS THE POINT, not defensiveness. ownerIsUser reads every value
// that is not "user" as an organization, so an unrecognized one — a typo in
// `--answer owner-type=`, a stale spelling in a script — would send a personal
// account to the org-scoped address, where GitHub answers 404 and the operator
// gets a blank App form instead of the prefilled one this run built. That is
// precisely the failure the question was added to remove, so it must not be
// reachable through a bad answer either.
func validateOwnerType(in string) error {
	switch strings.TrimSpace(in) {
	case ownerTypeOrg, ownerTypeUser:
		return nil
	case "":
		return fmt.Errorf("the App's owner must be declared as %q (an organization) or %q (a personal account)",
			ownerTypeOrg, ownerTypeUser)
	default:
		return fmt.Errorf("%q is not a GitHub App owner type; it must be %q or %q", in, ownerTypeOrg, ownerTypeUser)
	}
}

// validateExternalBaseURL refuses an address GitHub cannot deliver a webhook
// to, which is a stricter question than whether it is a URL.
//
// The shape check alone let `http://127.0.0.1:8080` through, and GitHub then
// refused the whole App with "Hook url is not supported because it isn't
// reachable over the public Internet" — AFTER the operator had gone to the
// browser, read a form and submitted it. That is the most expensive possible
// moment to learn it: the round trip is spent, and nothing this side can undo
// it. The value is not malformed, it is unreachable, so the refusal says so.
//
// EARLIEST PLACE THIS CAN BITE, AND IT IS NOT THE QUESTION. Both routes reach
// it before the browser opens — manifestParams runs inside HandoffSpec.Begin,
// which the client calls ahead of explain/openBrowser, and Result covers the
// route with no handoff at all. The question itself cannot carry it:
// channelkinds.ValidateInputs deliberately refuses Question.Validation for a
// channel wizard's inputs because nothing on this side evaluates CEL, so there
// is no per-field validator to hang this on and the operator re-runs rather
// than being re-asked in place. That is a real gap and it is the same one
// channelwizard.Run already documents for the name-collision check.
func validateExternalBaseURL(in string) error {
	base := strings.TrimSpace(in)
	if base == "" {
		return errors.New("an external base URL is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%q must be an absolute URL with a scheme and host, e.g. \"https://ap.example.com\"", base)
	}
	if why := channelkinds.UnreachableFromInternet(u.Hostname()); why != "" {
		return fmt.Errorf("%q cannot receive GitHub's webhooks: %s. GitHub delivers every event to "+
			"this address from the public Internet and refuses an App whose hook URL it cannot reach, "+
			"so this is checked here rather than left for GitHub to reject after you have filled the "+
			"form in. Give the address this cluster answers on from OUTSIDE it — for a cluster on your "+
			"own machine that means a public tunnel, which this project keeps for you: `oap install` "+
			"opens one on a local cluster, and `oap agent install` opens one on demand for a bundle "+
			"that declares a channel like this one. Both need an ngrok authtoken — export "+
			"NGROK_AUTHTOKEN and re-run — and both publish the tunnel's URL into the %s ConfigMap, "+
			"which is where this answer is seeded from.",
			base, why, spiceboxv1alpha1.WebdExternalURLConfigMap)
	}
	return nil
}

// ---------------------------------------------------------------------------
// the reference manifest and the steps around it, for the fallback route
// ---------------------------------------------------------------------------

// savedManifest is what became of the reference-manifest copy left on disk —
// same shape and same reason as the slack wizard's type of the same name: a
// failed save must not end the run (the manifest is still in the text the
// operator is reading), so it is carried and reported rather than returned as
// an error.
type savedManifest struct {
	path     string
	replaced bool
	err      error
}

const (
	manifestFileStem = "github-app-manifest-"
	manifestFileExt  = ".json"
	// manifestFilePerm is deliberately world-readable: the file is a
	// reference config (permissions, event names, URLs) and carries no
	// credential of any kind — the App's actual secrets never touch disk.
	manifestFilePerm = 0o644
)

func saveManifest(dir, channelName, manifest string) savedManifest {
	name := manifestFileName(channelName)
	path := name
	display := "./" + name
	if dir != "" {
		path = filepath.Join(dir, name)
		display = path
	}
	// A Stat error other than "not there" is not evidence the file is
	// absent, so it is read as "something is there" — the honest direction
	// for a claim the user is being asked to trust.
	_, statErr := os.Stat(path)
	replaced := statErr == nil || !os.IsNotExist(statErr)

	if err := os.WriteFile(path, []byte(manifest), manifestFilePerm); err != nil {
		return savedManifest{path: display, err: err}
	}
	return savedManifest{path: display, replaced: replaced}
}

func manifestFileName(channelName string) string {
	var b strings.Builder
	b.Grow(len(channelName))
	for _, r := range channelName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "github-app"
	}
	return manifestFileStem + name + manifestFileExt
}

func (s savedManifest) screenLines() string {
	// The zero value: nothing was written and nothing was attempted, because
	// the client offered nowhere to write (channelkinds.WizardInput.WorkingDir
	// is empty — a server rendering this wizard, for one). Saying "a copy is
	// saved at" with no path would send the operator looking for a file that
	// does not exist. The manifest itself is still on the screen either way.
	if s.err == nil && s.path == "" {
		return ""
	}
	if s.err != nil {
		return "This reference is not saved to\n  " + s.path + "\n  (" + s.err.Error() + ")\nso copy it from this screen before moving on."
	}
	if s.replaced {
		return "A copy is saved at\n  " + s.path + "\n  (replacing the file that was already there)."
	}
	return "A copy is saved at\n  " + s.path
}

func appManualSteps(createURL, manifestJSON string, saved savedManifest) string {
	var b strings.Builder
	b.WriteString("Create the GitHub App by hand, using this as a\n")
	b.WriteString("reference for what to enter:\n\n")
	if lines := saved.screenLines(); lines != "" {
		b.WriteString(lines)
		b.WriteString("\n\n")
	}
	b.WriteString("  1. " + createURL + "\n")
	b.WriteString("  2. Fill in the fields from the reference below —\n")
	b.WriteString("     name, homepage URL, webhook URL, permissions,\n")
	b.WriteString("     events — and CHOOSE a webhook secret (GitHub\n")
	b.WriteString("     never shows one back to you, so note what you\n")
	b.WriteString("     type; the field below asks for it).\n")
	b.WriteString("  3. Create the App, then \"Generate a private key\"\n")
	b.WriteString("     on its settings page — this downloads a .pem file.\n\n")
	b.WriteString(manifestJSON)
	return b.String()
}

// existingAppSteps is what an operator who BROUGHT a GitHub App reads above
// the questions asking for its details.
//
// IT SAYS THE AUTHORITY DIFFERENCE OUT LOUD, and that sentence is the reason
// this text exists rather than the other route's being reworded. The operator
// is about to hand over an App's private key, and "I already have one"
// silently meaning "and this cluster may now rewrite its settings" would be
// the worst possible reading of the choice they just made. So they are told
// the opposite explicitly: nothing here is written back to GitHub, and a
// webhook URL that does not match is REPORTED on the Channel, never
// corrected. The controller enforces it (its provisionedByThisTool gate); this
// is where the operator learns it.
//
// The reference manifest below the steps is the same one the create-it-by-hand
// route shows, and for the same reason: the webhook URL, the four permissions
// and the single event are what this Channel needs the App to BE, and none of
// them can be guessed. Here it is a checklist against an App that already
// exists rather than a form to fill in.
func existingAppSteps(settingsURL, webhookURL, manifestJSON string, saved savedManifest) string {
	var b strings.Builder
	b.WriteString("You already have the App, so nothing is created here.\n")
	b.WriteString("THIS RUN NEVER CHANGES ITS SETTINGS: an App it did not\n")
	b.WriteString("register is yours, so if its webhook URL does not match\n")
	b.WriteString("the one below, this Channel reports the drift and leaves\n")
	b.WriteString("the App alone rather than repointing it.\n\n")
	b.WriteString("Read its details off its settings page:\n\n")
	b.WriteString("  1. " + settingsURL + "\n")
	b.WriteString("  2. Open the App. Its ID and slug are the two values\n")
	b.WriteString("     the fields below ask for.\n")
	b.WriteString("  3. Check that its webhook URL is\n")
	b.WriteString("     " + webhookURL + "\n")
	b.WriteString("     and that it has the permissions and the event in\n")
	b.WriteString("     the reference below — set them if it does not.\n")
	b.WriteString("  4. \"Generate a private key\" downloads the .pem the\n")
	b.WriteString("     field below asks for the path to. GitHub never shows\n")
	b.WriteString("     an existing key again, so generate a new one.\n")
	b.WriteString("  5. The webhook secret is the value already set in the\n")
	b.WriteString("     App's \"Webhook secret\" field; set a new one there if\n")
	b.WriteString("     you do not have it.\n\n")
	if lines := saved.screenLines(); lines != "" {
		b.WriteString(lines)
		b.WriteString("\n\n")
	}
	b.WriteString(manifestJSON)
	return b.String()
}

func validateAppID(in string) error {
	id := strings.TrimSpace(in)
	if id == "" {
		return errors.New("an App ID is required")
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return fmt.Errorf("%q is not a valid App ID (expected a number)", id)
	}
	return nil
}

// readPrivateKeyPEM reads and lightly validates the App's downloaded private
// key file. Modeled on kubectl_kubeconfig's readKubeconfig
// (pkg/platform/identity/setup/builtins/kubectl_kubeconfig/flow.go): huh's
// line-oriented renderer — the one used off-TTY, under NO_COLOR, and by
// every test — reads exactly one line per field, so a pasted multi-line PEM
// document would arrive as its first line and nothing else, and that
// renderer has no error channel with which to say so. A path is also the
// only form a --non-interactive caller can supply through a single flag
// value without embedding literal newlines.
func readPrivateKeyPEM(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", errors.New("a path to the downloaded private key (.pem) is required")
	}
	resolved, err := expandHomePath(p)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("could not read the private key: %w", err)
	}
	pem := string(raw)
	if !strings.Contains(pem, "PRIVATE KEY") {
		return "", fmt.Errorf("%s does not look like a PEM private key (no \"PRIVATE KEY\" block found)", resolved)
	}
	return pem, nil
}

// expandHomePath resolves a leading ~ against the user's home directory, the
// same as kubectl_kubeconfig's expandPath: shells do this before a program
// ever sees an argument, so a path TYPED at a prompt is the one place it
// still has to be done by hand.
func expandHomePath(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not resolve %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

func installationSteps(installURL string) string {
	var b strings.Builder
	b.WriteString("Install the App to its owner, choosing which\n")
	b.WriteString("repositories it can access:\n\n  ")
	b.WriteString(installURL)
	b.WriteString("\n\n")
	b.WriteString("After installing, click \"Configure\" on the\n")
	b.WriteString("installation — the resulting URL ends in the\n")
	b.WriteString("installation ID this screen asks for.")
	return b.String()
}

func validateInstallationID(in string) error {
	id := strings.TrimSpace(in)
	if id == "" {
		return errors.New("an installation ID is required")
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return fmt.Errorf("%q is not a valid installation ID (expected a number)", id)
	}
	return nil
}
