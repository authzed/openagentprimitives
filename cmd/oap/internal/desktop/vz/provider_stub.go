//go:build !(darwin && arm64)

package vz

import (
	"context"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

type Provider struct{}

func New(_ desktop.VMConfig) *Provider { return &Provider{} }

func (*Provider) Provision(context.Context) error { return desktop.ErrUnsupportedPlatform }
func (*Provider) Start(context.Context) error     { return desktop.ErrUnsupportedPlatform }
func (*Provider) Stop(context.Context) error      { return desktop.ErrUnsupportedPlatform }
func (*Provider) Destroy(context.Context) error   { return desktop.ErrUnsupportedPlatform }
func (*Provider) Status(context.Context) (desktop.State, error) {
	return desktop.StateUnknown, desktop.ErrUnsupportedPlatform
}
func (*Provider) GuestIP(context.Context) (string, error) { return "", desktop.ErrUnsupportedPlatform }
func (*Provider) Kubeconfig(context.Context, string) ([]byte, error) {
	return nil, desktop.ErrUnsupportedPlatform
}

var _ desktop.ClusterProvider = (*Provider)(nil)
