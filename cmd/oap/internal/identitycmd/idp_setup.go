package identitycmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	// Blank imports register the idp kinds.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/idpscreens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
)

// idpSetupPollInterval is the polling interval for the Valid condition.
// Injectable for fast tests.
var idpSetupPollInterval = time.Second

// idpSetupPollTimeout is how long we wait for the Valid condition.
var idpSetupPollTimeout = 30 * time.Second

func newIdpSetupCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "setup <kind>",
		Short: "Configure the cluster identity provider interactively",
		Long: `Walk through an interactive wizard to configure the cluster identity provider.

<kind> is one of the registered provider kinds (e.g. google, oidc).
Run without arguments to see available kinds.

This command has no scripted form: the sign-in policy must be chosen
explicitly, and every other answer is a credential, which does not belong in
shell history or the process table.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			return RunIdpSetup(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), g, b.Controller, args[0])
		},
	}
}

func RunIdpSetup(ctx context.Context, stdin io.Reader, stdout io.Writer, g *apcmd.Globals, c client.Client, kindName string) error {
	k, ok := registry.Get(kindName)
	if !ok {
		return fmt.Errorf("unknown identity provider kind %q; available: %s", kindName, strings.Join(registry.Names(), ", "))
	}

	baseURL, err := clilogin.ResolveIdentitydBaseURL(ctx, c)
	if err != nil {
		fmt.Fprintln(stdout, "No external URL is configured. For a local cluster, run `oap init --local` first (it publishes the localhost URL); for remote users, configure ingress or an ngrok reserved domain. Then re-run `oap idp setup`.")
		return err
	}
	callbackURL := baseURL + "/oidc/callback/idp"

	// Load the existing provider so the wizard can pre-fill non-secret fields
	// and offer keep-on-blank for the secret. Only pre-fill when the existing
	// kind matches the requested kind.
	var existingSpec *spiceboxv1alpha1.ClusterIdentityProviderSpec
	var secretExists bool
	var existingCR spiceboxv1alpha1.ClusterIdentityProvider
	switch err := c.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &existingCR); {
	case err == nil:
		if existingCR.Spec.Kind == kindName {
			existingSpec = &existingCR.Spec
			if name := existingCR.Spec.ClientSecretRef.Name; name != "" {
				var sec corev1.Secret
				if serr := c.Get(ctx, types.NamespacedName{Name: name, Namespace: externalurl.Namespace}, &sec); serr == nil {
					secretExists = true
				} else if !apierrors.IsNotFound(serr) {
					return fmt.Errorf("probe existing client-secret Secret %q: %w", name, serr)
				}
			}
		}
	case apierrors.IsNotFound(err):
		// first setup — no existing provider
	default:
		return fmt.Errorf("load existing ClusterIdentityProvider: %w", err)
	}

	// Capabilities come from the writer this command was handed rather than
	// from os.Stdout: a test or a shell pipeline supplies a plain io.Writer,
	// and taking over a screen the command is not actually writing to would
	// leave the wizard invisible. In production the two are the same file.
	theme := g.Theme(stdout)

	wizardOut, answered, err := runIdpWizard(ctx, k.Wizard(), idp.WizardInput{
		CallbackURL:  callbackURL,
		Existing:     existingSpec,
		SecretExists: secretExists,
	}, kindName, IdpSetupOptions(theme, stdin, stdout, kindName, callbackURL))
	if err != nil {
		return err
	}

	// Fill in the operator namespace for the secret reference.
	wizardOut.Spec.ClientSecretRef.Namespace = externalurl.Namespace

	// Apply the Secret only when the wizard produced new material; nil SecretData
	// means "keep the existing Secret untouched" (re-setup with unchanged secret).
	if wizardOut.SecretData != nil {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      wizardOut.SecretName,
				Namespace: externalurl.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: wizardOut.SecretData,
		}
		if err := ApplySecret(ctx, c, secret); err != nil {
			return fmt.Errorf("apply secret %q: %w", wizardOut.SecretName, err)
		}
	}

	// Apply ClusterIdentityProvider "default".
	if err := ApplyClusterIdentityProvider(ctx, c, wizardOut.Spec); err != nil {
		return fmt.Errorf("apply ClusterIdentityProvider: %w", err)
	}

	// The summary is the record the run leaves behind — bubbletea's frame is
	// not one, whichever buffer it drew in — so it is written to the same
	// stream the run used, after the point of no return: the CR and its Secret
	// are already applied, so a failure to print it must not be reported as a
	// failure to configure the provider. It goes before the poll so the record
	// reads in the order things happened.
	if err := tui.RenderSummary(stdout, theme, answered.Notes()); err != nil {
		fmt.Fprintf(stdout, "%s\n", theme.Render(theme.Warn,
			"the identity provider was configured, but its summary could not be displayed: "+err.Error()))
	}

	// Poll for Valid condition.
	fmt.Fprint(stdout, "Waiting for identity provider to become valid...")
	pollErr := wait.Until(ctx, idpSetupPollInterval, idpSetupPollTimeout, func(ctx context.Context) (bool, error) {
		var cidp spiceboxv1alpha1.ClusterIdentityProvider
		if err := c.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &cidp); err != nil {
			return false, nil
		}
		for _, cond := range cidp.Status.Conditions {
			if cond.Type == spiceboxv1alpha1.ConditionIdPValid && cond.Status == metav1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	if pollErr != nil {
		fmt.Fprintln(stdout, " timed out.")
		// Print the current condition reason/message.
		var cidp spiceboxv1alpha1.ClusterIdentityProvider
		if err := c.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &cidp); err == nil {
			for _, cond := range cidp.Status.Conditions {
				if cond.Type == spiceboxv1alpha1.ConditionIdPValid {
					fmt.Fprintf(stdout, "  Reason:  %s\n", cond.Reason)
					fmt.Fprintf(stdout, "  Message: %s\n", cond.Message)
				}
			}
		}
		fmt.Fprintln(stdout, "Run `oap idp status` to see the current state.")
		return pollErr
	}
	fmt.Fprintln(stdout, " ready.")
	fmt.Fprintln(stdout, "Identity provider configured and valid.")
	return nil
}

// IdpSetupOptions is how this command's wizard is presented.
//
// Split out because Inline is not visible in the output of any driver a test
// can build, so this is the only place the decision below can be read back.
//
// Inline exactly when the redirect URI is printed to the stream — the same
// !Fits condition runIdpWizard prints on, so the flag and the print cannot
// disagree.
//
// The rule turns on whether an answer depends on output this run does not
// render. When the URI fits, it is IN the wizard's own note, so every answer
// can be given from what the form shows and the alternate screen costs nothing.
// When it does not fit, the note can carry only a cut copy — and a cut redirect
// URI registers nothing — so runIdpWizard prints the whole one above the form,
// and the operator needs it readable while the questions are up.
//
// This wizard is the only inline surface with a rail, which is why the
// classification is worth being exact about rather than always-on: with
// huh.Form.View empty while quitting, each screen of an inline railed run
// leaves a frame of empty chrome in scrollback.
func IdpSetupOptions(theme *tui.Theme, stdin io.Reader, stdout io.Writer, kindName, callbackURL string) tui.Options {
	return tui.Options{
		Theme: theme,
		Title: "oap · idp setup · " + kindName,
		In:    stdin,
		Out:   stdout,
		Inline: callbackURL != "" &&
			!idpscreens.CallbackAddress(callbackURL).Fits(tui.RailedNoteBudget()),
	}
}

// runIdpWizard asks a kind's wizard for its screens, presents them, and reads
// the CR and Secret material back out of the answered State. It returns that
// State so the caller can render the summary the screens accumulated.
//
// There is deliberately no non-interactive mode here, and so no fail-closed
// driver and no --answer: every question this command asks either IS a
// credential (a client secret, an admin password) or decides who may sign in to
// the cluster, and a flag value lands in shell history and in the process
// table. Nor does any screen open a browser or probe the network — the
// readiness probe is the validity controller's, server-side — so there is no
// step for a RequiresInteraction declaration to refuse up front.
func runIdpWizard(
	ctx context.Context,
	w idp.Wizard,
	wizIn idp.WizardInput,
	kindName string,
	opts tui.Options,
) (idp.WizardOutput, *tui.State, error) {
	st := tui.NewState()

	screens, err := w.Screens(ctx, wizIn)
	if err != nil {
		return idp.WizardOutput{}, st, err
	}
	if len(screens) == 0 {
		// Defense in depth against a kind that returns neither screens nor an
		// error: running zero screens would apply whatever empty spec Result
		// derived from an unanswered State.
		return idp.WizardOutput{}, st, fmt.Errorf("identity provider kind %q has no setup questions", kindName)
	}

	// The redirect URI is the one thing the user must carry to their provider's
	// console, and a note cuts an address it cannot hold whole — enough to
	// recognise, not enough to paste. When it does not fit, this is where the
	// whole one goes: on the stream, above the form, where a terminal
	// soft-wraps it and a copy still yields one usable line. It stays readable
	// while the questions are up because this run is inline; it is NOT dead
	// compensation for a screen that used to cover it, because what the note
	// carries in that case is a cut copy and a cut redirect URI registers
	// nothing.
	if addr := idpscreens.CallbackAddress(wizIn.CallbackURL); wizIn.CallbackURL != "" && !addr.Fits(tui.RailedNoteBudget()) {
		fmt.Fprintf(opts.Out, "\nAdd this redirect URI to the OAuth client you register:\n\n  %s\n\n", wizIn.CallbackURL)
	}

	answered, err := tui.RunWith(ctx, screens, opts, st)
	if err != nil {
		// Stripped here, at the one call site that produced the framing: `tui:
		// apply screen "issuer":` in front of a message about a malformed URL
		// is this command's plumbing showing through, including a screen ID
		// that names a step in the sequencer and nothing a user can act on.
		return idp.WizardOutput{}, answered, tui.UserFacing(err)
	}

	out, err := w.Result(answered)
	return out, answered, err
}

// ApplySecret creates or updates the Secret in-place.
func ApplySecret(ctx context.Context, c client.Client, secret *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, secret)
	}
	if err != nil {
		return err
	}
	existing.Type = secret.Type
	existing.Data = secret.Data
	return c.Update(ctx, existing)
}

// ApplyClusterIdentityProvider creates or replaces-spec on ClusterIdentityProvider "default".
func ApplyClusterIdentityProvider(ctx context.Context, c client.Client, spec spiceboxv1alpha1.ClusterIdentityProviderSpec) error {
	name := spiceboxv1alpha1.ClusterIdentityProviderName
	existing := &spiceboxv1alpha1.ClusterIdentityProvider{}
	err := c.Get(ctx, types.NamespacedName{Name: name}, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, &spiceboxv1alpha1.ClusterIdentityProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		})
	}
	if err != nil {
		return err
	}
	existing.Spec = spec
	return c.Update(ctx, existing)
}
