//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/agentcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

func newDesktopOapWorkflow(ctx context.Context, kb *kube.Bundle, out io.Writer, root *oap.Bundle, namespace, sourceDigest string) *install.Workflow {
	capacity := agentcmd.NewCapacityQuestions(ctx, kb)
	rootName := root.Manifest.Agent.Name
	return &install.Workflow{
		Client: kb.Controller,
		Options: install.InstallOpts{
			Namespace:           namespace,
			SourceKind:          "file",
			SourceDigest:        sourceDigest,
			Interactive:         false,
			AdoptSecretsAllowed: true,
		},
		Hooks: install.WorkflowHooks{
			CapacityQuestions: func(hookCtx context.Context, node install.NodeContext, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				questions, notices, err := capacity(hookCtx, crs)
				for _, notice := range notices {
					fmt.Fprintf(out, "install %s: %s\n", desktopGraphPath(rootName, node.Path), notice)
				}
				return questions, notices, err
			},
			AdoptDecision: func(_ context.Context, node install.NodeContext, conflicts []install.Conflict) ([]string, error) {
				title, message := adoptDialogText(conflicts)
				message = fmt.Sprintf("Agent path: %s\n\n%s", desktopGraphPath(rootName, node.Path), message)
				if !desktopAdoptDialog(title, message) {
					return nil, nil
				}
				keys := make([]string, 0, len(conflicts))
				for _, conflict := range conflicts {
					keys = append(keys, conflict.Key())
				}
				return keys, nil
			},
		},
	}
}

func desktopGraphPath(root string, path oap.DependencyPath) string {
	if len(path) == 0 {
		return root
	}
	return root + " > " + path.String()
}

// writeDesktopGraphResult reports only graph identity and non-secret result
// metadata. Question answers and synthesized Secret values never enter the
// GraphResult and are therefore not available to this formatter.
func writeDesktopGraphResult(out io.Writer, result *install.GraphResult) {
	if result == nil {
		return
	}
	nodes := slices.Clone(result.Nodes)
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Path.String() < nodes[j].Path.String() })
	for _, node := range nodes {
		if node.Result == nil {
			continue
		}
		fmt.Fprintf(out, "install %s: installed %s (kinds=%v secrets=%d adopted=%v)\n",
			desktopGraphPath(result.RootName, node.Path), node.Name,
			node.Result.AppliedKinds, node.Result.SecretsCreated, node.Result.Adopted)
		for _, warning := range node.Result.Warnings {
			fmt.Fprintf(out, "install %s: %s\n", desktopGraphPath(result.RootName, node.Path), warning)
		}
	}
	for _, notice := range result.Notices {
		fmt.Fprintf(out, "warning: %s\n", notice)
	}
}

func desktopRootResult(result *install.GraphResult) *install.Result {
	if result == nil {
		return nil
	}
	var root *install.Result
	for _, node := range result.Nodes {
		if len(node.Path) == 0 {
			root = node.Result
			break
		}
	}
	if root == nil {
		return nil
	}
	copy := *root
	copy.AppliedKinds = slices.Clone(root.AppliedKinds)
	copy.Adopted = slices.Clone(root.Adopted)
	copy.Warnings = slices.Clone(root.Warnings)
	copy.Warnings = append(copy.Warnings, result.Notices...)
	for _, node := range result.Nodes {
		if len(node.Path) == 0 || node.Result == nil {
			continue
		}
		copy.SecretsCreated += node.Result.SecretsCreated
		copy.Adopted = append(copy.Adopted, node.Result.Adopted...)
		for _, warning := range node.Result.Warnings {
			copy.Warnings = append(copy.Warnings, fmt.Sprintf("%s: %s", desktopGraphPath(result.RootName, node.Path), warning))
		}
	}
	sort.Strings(copy.Adopted)
	copy.Adopted = slices.Compact(copy.Adopted)
	copy.Warnings = slices.DeleteFunc(copy.Warnings, func(warning string) bool { return strings.TrimSpace(warning) == "" })
	return &copy
}
