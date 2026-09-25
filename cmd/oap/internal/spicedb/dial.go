package spicedb

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	authzspicedb "github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// How the CLI reaches the SpiceDB an `oap install` put in the cluster: the
// Service to port-forward, the label selector that finds its pods, the gRPC
// port, and the Secret holding the preshared token.
const (
	ServiceName = "spicebox-spicedb"
	Selector    = "authzed.com/cluster-component=spicedb"
	TargetPort  = 50051
	TokenSecret = "spicebox-spicedb-token"
	TokenKey    = "token"

	// SystemNamespace is where oap install puts the platform's own workloads,
	// SpiceDB among them. The Secret is read from there regardless of which
	// namespace the command is otherwise operating in.
	SystemNamespace = apcmd.SystemNamespace
)

// DialViaPortForward opens a SpiceDB client against the in-cluster SpiceDB by
// port-forwarding its Service and reading the preshared token from the
// install-time Secret.
//
// The returned cleanup func is non-nil on success and MUST be called: it
// closes the client and tears the port-forward down.
func DialViaPortForward(ctx context.Context, b *kube.Bundle) (*authzspicedb.Client, func(), error) {
	pf, err := portforward.New(b.REST, SystemNamespace, ServiceName, Selector, TargetPort, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := pf.Start(ctx, io.Discard); err != nil {
		return nil, nil, fmt.Errorf("port-forward SpiceDB: %w", err)
	}
	var tok corev1.Secret
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: SystemNamespace, Name: TokenSecret,
	}, &tok); err != nil {
		pf.Stop()
		return nil, nil, fmt.Errorf("get SpiceDB token Secret: %w", err)
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", pf.LocalPort())
	cli, err := authzspicedb.NewClient(endpoint, string(tok.Data[TokenKey]), true)
	if err != nil {
		pf.Stop()
		return nil, nil, fmt.Errorf("spicedb client: %w", err)
	}
	cleanup := func() {
		_ = cli.Close()
		pf.Stop()
	}
	return cli, cleanup, nil
}
