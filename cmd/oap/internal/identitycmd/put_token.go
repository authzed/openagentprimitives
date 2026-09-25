package identitycmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/dotenv"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

func newIdentityPutTokenCmd(g *apcmd.Globals) *cobra.Command {
	var (
		credName    string
		fromLiteral string
		fromEnv     string
		fromEnvFile string
		fromFile    string
		fromStdin   bool
		skipVerify  bool
	)

	cmd := &cobra.Command{
		Use:   "put-token <identity-name>",
		Short: "Set the Secret value behind an AgentIdentity credential",
		Long: `Read an existing AgentIdentity, look up the named credential's
secretRef, and create-or-update the underlying Secret with the supplied
token bytes. Lets you keep AgentIdentity YAML in git without secrets.

Exactly one of --from-literal, --from-env, --from-env-file, --from-file,
--from-stdin must be supplied.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIdentityPutToken(cmd.Context(), cmd.OutOrStdout(), g, args[0],
				credName, fromLiteral, fromEnv, fromEnvFile, fromFile, fromStdin, skipVerify)
		},
	}
	cmd.Flags().StringVar(&credName, "credential", "",
		"Name of the credential within the AgentIdentity (required)")
	cmd.Flags().StringVar(&fromLiteral, "from-literal", "",
		"Token value as a literal string (avoid in shell history)")
	cmd.Flags().StringVar(&fromEnv, "from-env", "",
		"Read token from the named environment variable (e.g. GITHUB_TOKEN)")
	cmd.Flags().StringVar(&fromEnvFile, "from-env-file", "",
		"Read token from a .env file: PATH:KEY (note: PATH may not contain a colon)")
	cmd.Flags().StringVar(&fromFile, "from-file", "",
		"Read token from a file path")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false,
		"Read token from stdin (single line; trailing newline trimmed)")
	cmd.Flags().BoolVar(&skipVerify, "skip-verify", false,
		"Skip live verification of the token against the provider (format check still applies)")
	_ = cmd.MarkFlagRequired("credential")
	return cmd
}

func runIdentityPutToken(
	ctx context.Context,
	out io.Writer,
	g *apcmd.Globals,
	identityName, credName, fromLiteral, fromEnv, fromEnvFile, fromFile string,
	fromStdin, skipVerify bool,
) error {
	tokenBytes, err := ResolveTokenSource(fromLiteral, fromEnv, fromEnvFile, fromFile, fromStdin)
	if err != nil {
		return err
	}

	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// When the token itself arrived on --from-stdin, stdin can't also carry
	// a confirmation, so the flow is non-interactive.
	interactive := !fromStdin && term.IsTerminal(int(os.Stdin.Fd()))
	return putTokenIntoIdentity(ctx, out, b.Controller, b.Namespace,
		identityName, credName, tokenBytes, skipVerify, os.Stdin, interactive)
}

// putTokenIntoIdentity is the client.Client-injectable core of the put-token
// command: it runs the format gate + live-verification gate, then writes the
// Secret behind the AgentIdentity's named static credential. Split out from
// runIdentityPutToken (which resolves the token source, bundle, and terminal
// state) so the gate + store behavior is unit-testable with a fake client.
func putTokenIntoIdentity(
	ctx context.Context,
	out io.Writer,
	c client.Client,
	ns, identityName, credName string,
	tokenBytes []byte,
	skipVerify bool,
	stdin io.Reader,
	interactive bool,
) error {
	// Closing an old gap: this path previously wrote the Secret with no
	// format check at all. Resolve the provider best-effort (toolkit
	// catalog → MCPServers); unknown credentials stay permissive.
	token := string(tokenBytes)
	var prov *provider.Provider
	if p, ok := passthroughcatalog.ProviderForCredential(ctx, c, credName); ok {
		prov = p
		if err := provider.ValidateToken(*p, token); err != nil {
			return fmt.Errorf("refusing to store credential %q: %w", credName, err)
		}
	}
	// The result's attested subject id is deliberately unused here: this path
	// writes an AGENT's Secret, and an AgentIdentity names no single human, so
	// there is no identity to bind. Only the UserIdentity routes record it —
	// see pkg/controllers/useridentity/attested_edge.go.
	if _, err := verifyGate(ctx, out, prov, builtins.StoreValue{Bearer: token}, skipVerify, stdin, interactive); err != nil {
		return fmt.Errorf("credential %q: %w", credName, err)
	}

	secretName, secretKey, created, err := storeTokenInSecret(ctx, c, ns, identityName, credName, tokenBytes)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "created Secret %s/%s with key %q (%d bytes)\n",
			ns, secretName, secretKey, len(tokenBytes))
	} else {
		fmt.Fprintf(out, "updated Secret %s/%s key %q (%d bytes)\n",
			ns, secretName, secretKey, len(tokenBytes))
	}
	return nil
}

// storeTokenInSecret resolves the AgentIdentity's named static credential to a
// Secret, then creates-or-updates that Secret with token. It returns the secret
// name, secret key, whether the Secret was created (true) or updated (false),
// and any error.
func storeTokenInSecret(
	ctx context.Context,
	c client.Client,
	ns, identityName, credName string,
	token []byte,
) (secretName, secretKey string, created bool, err error) {
	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: identityName}, &ai); err != nil {
		return "", "", false, fmt.Errorf("get AgentIdentity %s/%s: %w", ns, identityName, err)
	}

	var cred *spiceboxv1alpha1.AgentCredential
	for i := range ai.Spec.Credentials {
		if ai.Spec.Credentials[i].Name == credName {
			cred = &ai.Spec.Credentials[i]
			break
		}
	}
	if cred == nil {
		return "", "", false, fmt.Errorf("AgentIdentity %s has no credential named %q", identityName, credName)
	}

	// Only a credential kind whose Secret holds a SINGLE pasteable value
	// under one named key can be put-token'd. A type whose Secret has a
	// fixed multi-key shape (oauth) reports a SecretRef with an empty Key; a
	// minted type (federated) reports no SecretRef at all — both refuse
	// here rather than writing into a Secret shape this command doesn't
	// understand.
	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return "", "", false, fmt.Errorf("AgentIdentity %s credential %q: %w", identityName, credName, err)
	}
	ref := k.SecretRef(*cred)
	if ref == nil || ref.Key == "" {
		return "", "", false, fmt.Errorf("AgentIdentity %s credential %q is type=%q (only a single-key credential can be put-token'd)",
			identityName, credName, cred.Type)
	}

	secretName = ref.Name
	secretKey = ref.Key

	created, err = dotenv.WriteSecretKey(ctx, c, ns, secretName, secretKey, token)
	return secretName, secretKey, created, err
}

// ResolveTokenSource resolves the token value from exactly one of the provided
// source flags. fromEnvFile is of the form "PATH:KEY".
func ResolveTokenSource(fromLiteral, fromEnv, fromEnvFile, fromFile string, fromStdin bool) ([]byte, error) {
	sources := 0
	if fromLiteral != "" {
		sources++
	}
	if fromEnv != "" {
		sources++
	}
	if fromEnvFile != "" {
		sources++
	}
	if fromFile != "" {
		sources++
	}
	if fromStdin {
		sources++
	}
	if sources != 1 {
		return nil, fmt.Errorf("exactly one of --from-literal, --from-env, --from-env-file, --from-file, --from-stdin must be supplied (got %d)", sources)
	}

	switch {
	case fromLiteral != "":
		return []byte(fromLiteral), nil
	case fromEnv != "":
		v := os.Getenv(fromEnv)
		if v == "" {
			return nil, fmt.Errorf("environment variable %s is empty or unset", fromEnv)
		}
		return []byte(v), nil
	case fromEnvFile != "":
		return resolveEnvFile(fromEnvFile)
	case fromFile != "":
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", fromFile, err)
		}
		return trimTrailingNewline(b), nil
	case fromStdin:
		r := bufio.NewReader(os.Stdin)
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return trimTrailingNewline(b), nil
	}
	return nil, fmt.Errorf("internal: no token source matched")
}

// resolveEnvFile parses a "PATH:KEY" spec and reads the value from the dotenv file.
func resolveEnvFile(spec string) ([]byte, error) {
	idx := strings.Index(spec, ":")
	if idx <= 0 || idx >= len(spec)-1 {
		return nil, fmt.Errorf("--from-env-file requires PATH:KEY (colon-separated); got %q", spec)
	}
	path := spec[:idx]
	key := spec[idx+1:]
	return dotenv.Read(path, key)
}

func trimTrailingNewline(b []byte) []byte {
	return []byte(strings.TrimRight(string(b), "\r\n"))
}
