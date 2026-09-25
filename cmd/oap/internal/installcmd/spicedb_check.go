package installcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/authzed/grpcutil"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func newSpiceDBCheckCmd(g *apcmd.Globals) *cobra.Command {
	endpoint := ""
	tokenFile := ""
	insecure := true

	cmd := &cobra.Command{
		Use:   "check <object_type>:<object_id>#<permission>@<subject_type>:<subject_id>[#<rel>]",
		Short: "One-shot CheckPermission query for debugging.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSpiceDBCheck(cmd.Context(), cmd.OutOrStdout(), endpoint, tokenFile, insecure, args[0])
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "SpiceDB gRPC endpoint (default: $SPICEDB_ENDPOINT)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "Path to bearer token file (default: $SPICEDB_TOKEN_FILE)")
	cmd.Flags().BoolVar(&insecure, "insecure", true, "Use insecure gRPC")
	return cmd
}

func runSpiceDBCheck(ctx context.Context, out io.Writer, endpoint, tokenFile string, insecure bool, query string) error {
	resObj, perm, sub, err := parseCheckQuery(query)
	if err != nil {
		return err
	}

	if endpoint == "" {
		endpoint = os.Getenv("SPICEDB_ENDPOINT")
	}
	if endpoint == "" {
		return fmt.Errorf("--endpoint is required")
	}
	token := ""
	if tokenFile == "" {
		tokenFile = os.Getenv("SPICEDB_TOKEN_FILE")
	}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}
		token = string(b)
	}

	opts := []grpc.DialOption{}
	if insecure {
		opts = append(opts, grpcutil.WithInsecureBearerToken(token))
	} else {
		opts = append(opts, grpcutil.WithBearerToken(token))
	}
	client, err := authzed.NewClient(endpoint, opts...)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	resp, err := client.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:   resObj,
		Permission: perm,
		Subject:    sub,
		Consistency: &v1.Consistency{
			Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true},
		},
	})
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}

	switch resp.GetPermissionship() {
	case v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION:
		fmt.Fprintln(out, "GRANTED")
	case v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION:
		fmt.Fprintln(out, "DENIED")
	default:
		fmt.Fprintln(out, "UNSPECIFIED")
	}
	return nil
}

func parseCheckQuery(q string) (*v1.ObjectReference, string, *v1.SubjectReference, error) {
	at := strings.Index(q, "@")
	if at < 0 {
		return nil, "", nil, fmt.Errorf("missing @ in query")
	}
	left, right := q[:at], q[at+1:]
	hash := strings.Index(left, "#")
	if hash < 0 {
		return nil, "", nil, fmt.Errorf("missing # before permission")
	}
	resStr, perm := left[:hash], left[hash+1:]
	colon := strings.Index(resStr, ":")
	if colon < 0 {
		return nil, "", nil, fmt.Errorf("missing : in resource")
	}
	res := &v1.ObjectReference{ObjectType: resStr[:colon], ObjectId: resStr[colon+1:]}

	sub := &v1.SubjectReference{}
	subRel := ""
	if h := strings.Index(right, "#"); h >= 0 {
		subRel = right[h+1:]
		right = right[:h]
	}
	colon = strings.Index(right, ":")
	if colon < 0 {
		return nil, "", nil, fmt.Errorf("missing : in subject")
	}
	sub.Object = &v1.ObjectReference{ObjectType: right[:colon], ObjectId: right[colon+1:]}
	sub.OptionalRelation = subRel

	return res, perm, sub, nil
}
