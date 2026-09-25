package installcmd

import (
	"context"
	"fmt"
	"io"
	"os"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/authzed/grpcutil"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	insecuregrpc "google.golang.org/grpc/credentials/insecure"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
)

func newSpiceDBApplySchemaCmd(g *apcmd.Globals) *cobra.Command {
	endpoint := ""
	tokenFile := ""
	insecure := true

	cmd := &cobra.Command{
		Use:   "apply-schema",
		Short: "Apply the canonical agentprimitives schema (pkg/authz/spicedb/schema) to the SpiceDB endpoint via WriteSchema (idempotent).",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSpiceDBApplySchema(cmd.Context(), cmd.OutOrStdout(), endpoint, tokenFile, insecure)
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "SpiceDB gRPC endpoint (default: $SPICEDB_ENDPOINT)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "Path to a file holding the SpiceDB bearer token (default: $SPICEDB_TOKEN_FILE)")
	cmd.Flags().BoolVar(&insecure, "insecure", true, "Use insecure gRPC (no TLS)")
	return cmd
}

// runSpiceDBApplySchema resolves the endpoint + token FILE (flag, else the
// matching env var) and delegates to applySchemaToEndpoint with the token
// read into a string. Kept as the file-based entry point for the `oap spicedb
// apply-schema` command and the SPICEDB_ENDPOINT-triggered step in `oap init`.
func runSpiceDBApplySchema(ctx context.Context, out io.Writer, endpoint, tokenFile string, insecure bool) error {
	if endpoint == "" {
		endpoint = os.Getenv("SPICEDB_ENDPOINT")
	}
	if endpoint == "" {
		return fmt.Errorf("--endpoint is required (or set SPICEDB_ENDPOINT)")
	}
	if tokenFile == "" {
		tokenFile = os.Getenv("SPICEDB_TOKEN_FILE")
	}
	token := ""
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}
		token = string(b)
	}
	return applySchemaToEndpoint(ctx, out, endpoint, token, insecure)
}

// applySchemaToEndpoint dials endpoint (with token as a bearer credential, TLS
// on unless insecure) and writes the canonical agentprimitives schema via
// WriteSchema (idempotent — SpiceDB accepts a re-apply of the same schema).
// This is the token-STRING core shared by the file-based `oap spicedb
// apply-schema` command (runSpiceDBApplySchema, above) and the external-
// SpiceDB post-install apply in `oap init` (which already holds the token as a
// string, not a file).
func applySchemaToEndpoint(ctx context.Context, out io.Writer, endpoint, token string, insecure bool) error {
	if endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	opts := []grpc.DialOption{}
	if insecure {
		opts = append(opts,
			grpcutil.WithInsecureBearerToken(token),
			grpc.WithTransportCredentials(insecuregrpc.NewCredentials()),
		)
	} else {
		opts = append(opts,
			grpcutil.WithBearerToken(token),
			grpc.WithTransportCredentials(credentials.NewTLS(nil)),
		)
	}
	client, err := authzed.NewClient(endpoint, opts...)
	if err != nil {
		return fmt.Errorf("dial spicedb: %w", err)
	}
	defer client.Close()

	resp, err := client.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: spicedb.Schema})
	if err != nil {
		return fmt.Errorf("write schema: %w", err)
	}
	fmt.Fprintf(out, "schema applied (write zedToken=%s)\n", resp.GetWrittenAt().GetToken())
	return nil
}
