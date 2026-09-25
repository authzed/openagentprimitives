package memclient

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
)

// Conn holds the resources returned by Connect. Callers must defer Close().
type Conn struct {
	Client *httpclient.Client
	Scope  memory.Scope
	pf     *portforward.PortForwarder
	token  string
}

func (c *Conn) Close() {
	if c.pf != nil {
		c.pf.Stop()
	}
}

// BaseURL is the port-forwarded operator base URL (no trailing slash).
func (c *Conn) BaseURL() string { return c.pf.URL() }

// Token is the per-session memory bearer token.
func (c *Conn) Token() string { return c.token }

// Connect resolves the memory bearer token from the session's K8s Secret,
// opens a port-forward to the operator, and returns a connected Conn.
// backend may be "" (default), "primary", or "secondary" to override
// the shadow backend's read source.
func Connect(ctx context.Context, b *kube.Bundle, sessionName, backend string) (*Conn, error) {
	var sess spiceboxv1alpha1.AgentSession
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: b.Namespace, Name: sessionName,
	}, &sess); err != nil {
		return nil, fmt.Errorf("get AgentSession %s/%s: %w", b.Namespace, sessionName, err)
	}

	var sec corev1.Secret
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: b.Namespace, Name: sessionName + "-memory-token",
	}, &sec); err != nil {
		return nil, fmt.Errorf("memory token not ready for session %q (is the session running?): %w",
			sessionName, err)
	}
	token := string(sec.Data["token"])
	if token == "" {
		return nil, fmt.Errorf("memory-token Secret %s-memory-token has empty 'token' key",
			sessionName)
	}

	pf, err := portforward.New(b.REST, "agentprimitives-system",
		"spicebox-operator", "app.kubernetes.io/name=spicebox-operator", 8082, 0)
	if err != nil {
		return nil, err
	}
	if err := pf.Start(ctx, io.Discard); err != nil {
		return nil, fmt.Errorf("port-forward: %w", err)
	}

	scope := memory.Scope{Kind: "session", ID: b.Namespace + "/" + sessionName}
	client := httpclient.New(pf.URL(), token)
	if backend != "" {
		client = client.WithBackend(backend)
	}
	return &Conn{
		Client: client,
		Scope:  scope,
		pf:     pf,
		token:  token,
	}, nil
}
