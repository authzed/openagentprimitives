// Package kubectl_kubeconfig is the builtin Go flow for the kubectl-kubeconfig
// provider. It takes the path of a kubeconfig file, reads it, checks it really
// is one, and stores the whole blob as a single static credential. Binding it to
// KUBECONFIG via a tmpfile is the runtime's job; this builtin only stores it.
//
// As a screen sequence:
//
//	kubeconfig   take the path of the file to store
//
// # Why a path and not a paste
//
// huh's line-oriented renderer — the one used off-TTY, under NO_COLOR, and by
// every test — reads exactly one line per field, so a pasted multi-line YAML
// document would arrive as its first line and nothing else, and that renderer
// has no error channel with which to say so. A path is also the only form a
// caller answering the flow from flags can supply.
package kubectl_kubeconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/flowscreens"
)

// KeyPath is the State key the kubeconfig's path lands under. Stable: a caller
// answering this flow ahead of time addresses the screen by it.
const KeyPath = "kubeconfig"

// Flow is the kubectl-kubeconfig builtin.
type Flow struct{}

// New returns a new Flow.
func New() *Flow { return &Flow{} }

// Name returns the registry name for this flow.
func (Flow) Name() string { return "kubectl-kubeconfig" }

// Screens describes the flow: one question, pre-filled with the kubeconfig this
// shell is already using.
//
// req.Provider.Prompt is LLM-system-prompt content for the fallback agent. Do
// NOT show it to the user; the guidance below is composed from structured fields.
func (Flow) Screens(_ context.Context, req builtins.Request) ([]tui.Screen, error) {
	return []tui.Screen{
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        "kubeconfig",
				Label:     "Kubeconfig",
				Key:       KeyPath,
				Title:     "Path to the kubeconfig file",
				Guidance:  func(*tui.State) string { return guidance(req) },
				Addresses: []tui.Address{flowscreens.DocsAddress(req.Provider)},
				// The path is not the credential — the file's contents are — so the
				// summary shows it plainly, with no NoteValue. Naming the file the user
				// chose is the only way they can tell a mis-typed path from the one
				// they meant.
				NoteLabel: "Kubeconfig",
			},
			Default: defaultPath,
			Check: func(_ *tui.State, v string) error {
				_, err := readKubeconfig(v)
				return err
			},
		}),
	}, nil
}

// Result reads the chosen file and stores its contents.
//
// It re-reads rather than carrying the bytes over from the screen, so what is
// stored is the file as it is at the moment of storing, and so a State answered
// from flags — where no screen ever ran — goes through the same checks.
func (Flow) Result(ctx context.Context, req builtins.Request, st *tui.State) error {
	if st == nil {
		return errors.New("kubectl-kubeconfig: no kubeconfig was supplied")
	}
	path := strings.TrimSpace(st.Get(KeyPath))
	if path == "" {
		return errors.New("kubectl-kubeconfig: no kubeconfig was supplied")
	}
	blob, err := readKubeconfig(path)
	if err != nil {
		return fmt.Errorf("kubectl-kubeconfig: %w", err)
	}
	if req.Store == nil {
		return errors.New("kubectl-kubeconfig: nowhere to store the kubeconfig")
	}
	return req.Store(ctx, builtins.StoreValue{KubeconfigYAML: blob})
}

// Verify reports unsupported: no live API-discovery ping against the
// kubeconfig's cluster is implemented.
func (Flow) Verify(ctx context.Context, req builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return builtins.VerifyResult{
		Status: builtins.VerifyUnsupported,
		Detail: "kubeconfig live verification not implemented; stored unverified",
	}, nil
}

// readKubeconfig resolves path, reads it, and checks it really is a kubeconfig.
// It is both the field's validator — so a wrong path is corrected where it was
// typed — and the read Result stores from, so the two cannot disagree.
func readKubeconfig(path string) (string, error) {
	resolved, err := expandPath(path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		// os.ReadFile's message already names the file and the reason, which is
		// what the person who typed the path needs.
		return "", fmt.Errorf("could not read the kubeconfig: %w", err)
	}
	blob := string(raw)
	if strings.TrimSpace(blob) == "" {
		return "", fmt.Errorf("%s is empty", resolved)
	}
	var probe struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("%s is not valid YAML: %w", resolved, err)
	}
	if probe.Kind != "Config" {
		return "", fmt.Errorf("%s is not a kubeconfig (its kind is %q, not \"Config\")", resolved, probe.Kind)
	}
	return blob, nil
}

// expandPath resolves a leading ~ against the user's home directory. Shells do
// this before a program ever sees an argument, so a path TYPED at a prompt is
// the one place it still has to be done by hand. Both separators are accepted:
// people type "~/.kube/config" on Windows too, and gating on os.PathSeparator
// alone would leave it unexpanded there and fail to open a file that exists.
func expandPath(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", errors.New("no path was supplied")
	}
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
	// filepath.Join normalises whichever separator was typed to the platform's own.
	return filepath.Join(home, p[2:]), nil
}

// defaultPath pre-fills the field with the kubeconfig this shell is already
// pointed at: KUBECONFIG when set, kubectl's own default otherwise.
//
// Only the first entry of a KUBECONFIG list is offered. The list means "merge
// these", which is not a single file this flow could store, so the first entry
// is a starting point the user can correct rather than a guess at the merge.
func defaultPath() string {
	if env := strings.TrimSpace(os.Getenv("KUBECONFIG")); env != "" {
		if first := strings.Split(env, string(os.PathListSeparator))[0]; strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

// guidance is what the user reads above the field. Every line is kept inside
// the note's column budget, which the package's tests assert.
func guidance(req builtins.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Choosing a kubeconfig for %q.\n\n", req.IdentityName)
	b.WriteString("The whole file is stored as the credential, so point\n")
	b.WriteString("this at one narrowed to the context you want the agent\n")
	b.WriteString("to have — not a merged config for every cluster you use.\n")
	return strings.TrimRight(b.String(), "\n")
}
