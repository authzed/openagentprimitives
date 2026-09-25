// Package apcmd holds the pieces every `oap` subcommand family needs: the
// persistent global flags, the kube-client seam built from them, and the
// generic cobra command factories the per-resource verbs are built out of.
//
// It exists so each command family can live in its own subpackage. A
// subpackage cannot import package main, so anything shared between the root
// command tree and a family — or between two families — has to live below
// both. This is that place.
package apcmd

import (
	"fmt"
	"os/exec"
	"strings"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// Globals captures flags shared by every subcommand. Subcommands read these
// via the closure pattern (capture *Globals at construction time).
type Globals struct {
	Namespace  string
	Kubeconfig string
	Context    string
	NoColor    bool

	// BundleFn, when non-nil, replaces the real kube-client construction below.
	// Production leaves it nil; tests inject a fake-backed *kube.Bundle so a
	// command's whole RunE path can run without a cluster. This is the only
	// seam — do NOT add per-command client parameters.
	BundleFn func() (*kube.Bundle, error)
}

// Bundle constructs a kube.Bundle from these Globals. Centralises the
// kube.New(kube.ClientOpts{Kubeconfig, Context, Namespace}) boilerplate
// every subcommand used to repeat. Production call sites should use
// this; tests that mint a Bundle directly stay unchanged.
func (g *Globals) Bundle() (*kube.Bundle, error) {
	if g.BundleFn != nil {
		return g.BundleFn()
	}
	return kube.New(kube.ClientOpts{
		Kubeconfig: g.Kubeconfig,
		Context:    g.Context,
		Namespace:  g.Namespace,
	})
}

// CurrentContext is the kubeconfig context this invocation targets: --context
// when given, otherwise whatever `kubectl config current-context` reports.
//
// It shells out rather than reading the kubeconfig because the answer must be
// the one kubectl itself would give — that is the context name the user reads
// back in every message this value appears in.
func (g *Globals) CurrentContext() (string, error) {
	if g.Context != "" {
		return g.Context, nil
	}
	out, err := exec.Command("kubectl", "config", "current-context").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

// KubeconfigRaw is the serialized kubeconfig this invocation resolves to, for
// the commands that hand a whole kubeconfig to something else (a build, a VM).
func (g *Globals) KubeconfigRaw() ([]byte, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if g.Kubeconfig != "" {
		rules.ExplicitPath = g.Kubeconfig
	}
	cfg, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return clientcmd.Write(*cfg)
}
