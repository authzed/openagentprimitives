package agentcmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/dotenv"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newAgentPutKeyCmd(g *apcmd.Globals) *cobra.Command {
	var (
		fromLiteral string
		fromEnv     string
		fromEnvFile string
		fromFile    string
		fromStdin   bool
	)

	cmd := &cobra.Command{
		Use:   "put-key <agent-class>",
		Short: "Set the model API key Secret for an AgentClass",
		Long: `Read an existing AgentClass, follow its spec.model.apiKey SecretKeyRef,
and create-or-update the underlying Secret with the supplied key bytes.
Lets you keep AgentClass YAML in git without embedding secrets.

The AgentClass must have spec.model.apiKey.name and spec.model.apiKey.key set.

Exactly one of --from-literal, --from-env, --from-env-file, --from-file,
--from-stdin must be supplied.

Example:
  oap agent put-key my-agent --from-env-file .env:ANTHROPIC_API_KEY`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentPutKey(cmd.Context(), cmd.OutOrStdout(), g, args[0],
				fromLiteral, fromEnv, fromEnvFile, fromFile, fromStdin)
		},
	}
	cmd.Flags().StringVar(&fromLiteral, "from-literal", "",
		"Key value as a literal string (avoid in shell history)")
	cmd.Flags().StringVar(&fromEnv, "from-env", "",
		"Read key from the named environment variable (e.g. ANTHROPIC_API_KEY)")
	cmd.Flags().StringVar(&fromEnvFile, "from-env-file", "",
		"Read key from a .env file: PATH:KEY (note: PATH may not contain a colon)")
	cmd.Flags().StringVar(&fromFile, "from-file", "",
		"Read key from a file path")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false,
		"Read key from stdin (single line; trailing newline trimmed)")
	return cmd
}

func runAgentPutKey(
	ctx context.Context,
	out io.Writer,
	g *apcmd.Globals,
	agentClassName, fromLiteral, fromEnv, fromEnvFile, fromFile string,
	fromStdin bool,
) error {
	keyBytes, err := identitycmd.ResolveTokenSource(fromLiteral, fromEnv, fromEnvFile, fromFile, fromStdin)
	if err != nil {
		return err
	}

	b, err := g.Bundle()
	if err != nil {
		return err
	}

	secretName, secretKey, created, err := storeAgentClassKey(ctx, b.Controller, b.Namespace, agentClassName, keyBytes)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "created Secret %s/%s with key %q (%d bytes)\n",
			b.Namespace, secretName, secretKey, len(keyBytes))
	} else {
		fmt.Fprintf(out, "updated Secret %s/%s key %q (%d bytes)\n",
			b.Namespace, secretName, secretKey, len(keyBytes))
	}
	return nil
}

// storeAgentClassKey reads the AgentClass, resolves its spec.model.apiKey
// SecretKeyRef, and creates-or-updates the underlying Secret.
func storeAgentClassKey(
	ctx context.Context,
	c client.Client,
	ns, agentClassName string,
	value []byte,
) (secretName, secretKey string, created bool, err error) {
	var ac spiceboxv1alpha1.AgentClass
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: agentClassName}, &ac); err != nil {
		return "", "", false, fmt.Errorf("get AgentClass %s/%s: %w", ns, agentClassName, err)
	}

	var apiKey spiceboxv1alpha1.SecretKeyRef
	switch {
	case ac.Spec.Model != nil:
		apiKey = ac.Spec.Model.APIKey
	case ac.Status.EffectiveSettings != nil && ac.Status.EffectiveSettings.Model.APIKey.Name != "":
		apiKey = ac.Status.EffectiveSettings.Model.APIKey
	default:
		return "", "", false, fmt.Errorf("AgentClass %s: model not configured on the class and no tier default with an apiKey is available — set spec.model or a settings-tier default",
			agentClassName)
	}
	if apiKey.Name == "" || apiKey.Key == "" {
		return "", "", false, fmt.Errorf("AgentClass %s spec.model.apiKey is not configured (name=%q, key=%q)",
			agentClassName, apiKey.Name, apiKey.Key)
	}

	created, err = dotenv.WriteSecretKey(ctx, c, ns, apiKey.Name, apiKey.Key, value)
	return apiKey.Name, apiKey.Key, created, err
}
