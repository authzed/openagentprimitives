package toolscmd

import (
	"context"
	"fmt"
	"reflect"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/kinds/registry"
)

// toolsListClientFactory is a package-level var so tests can stub it.
var toolsListClientFactory = defaultToolsClient

func newToolsListCmd(g *apcmd.Globals) *cobra.Command {
	var allNamespaces bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all tool resources across registered kinds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, ns, err := toolsListClientFactory(g)
			if err != nil {
				return err
			}
			rows, err := listAllKinds(cmd.Context(), c, ns, allNamespaces)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			return toolscli.RenderRows(out, g.Theme(out), rows)
		},
	}
	apcmd.AllNamespacesFlag(cmd, &allNamespaces)
	return cmd
}

// listAllKinds walks every registered kind, performs a List, and converts
// items into contract.Row.
func listAllKinds(ctx context.Context, c client.Client, ns string, allNamespaces bool) ([]contract.Row, error) {
	var rows []contract.Row
	for _, k := range registry.All() {
		listObj := k.NewList()
		var opts []client.ListOption
		// Cluster-scoped kinds ignore namespace; only restrict namespaced
		// kinds to the resolved namespace.
		clusterScoped := false
		if cs, ok := k.(contract.ClusterScoped); ok && cs.ClusterScoped() {
			clusterScoped = true
		}
		if !allNamespaces && ns != "" && !clusterScoped {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := c.List(ctx, listObj, opts...); err != nil {
			return nil, fmt.Errorf("list %s: %w", k.Name(), err)
		}
		items, err := extractItems(listObj)
		if err != nil {
			return nil, fmt.Errorf("extract %s items: %w", k.Name(), err)
		}
		for _, it := range items {
			rows = append(rows, k.Row(it))
		}
	}
	return rows, nil
}

// extractItems pulls .Items out of a typed ObjectList via reflection. Every
// generated *List type has an Items slice of concrete CR pointers (or
// values).
func extractItems(list client.ObjectList) ([]client.Object, error) {
	v := reflect.ValueOf(list)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	itemsField := v.FieldByName("Items")
	if !itemsField.IsValid() || itemsField.Kind() != reflect.Slice {
		return nil, fmt.Errorf("no Items slice on %T", list)
	}
	out := make([]client.Object, 0, itemsField.Len())
	for i := 0; i < itemsField.Len(); i++ {
		el := itemsField.Index(i)
		if el.Kind() == reflect.Ptr {
			out = append(out, el.Interface().(client.Object))
			continue
		}
		out = append(out, el.Addr().Interface().(client.Object))
	}
	return out, nil
}

// defaultToolsClient builds the standard kube bundle and returns the typed
// controller-runtime client + the resolved namespace.
func defaultToolsClient(g *apcmd.Globals) (client.Client, string, error) {
	b, err := g.Bundle()
	if err != nil {
		return nil, "", err
	}
	return b.Controller, b.Namespace, nil
}
