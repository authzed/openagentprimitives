// Package settingscmd implements `oap settings`: the cluster-wide agent
// security-defaults wizard and the apply verb behind it.
package settingscmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// DynamicFactory is the single dynamic-client factory shared by every
// settings subcommand (apply, wizard). It is a package-level var so tests can
// stub it, mirroring toolsListClientFactory for the tools subcommands. DynIface
// is declared once in settings_apply.go.
var DynamicFactory = defaultSettingsDynamic

func defaultSettingsDynamic(g *apcmd.Globals) (DynIface, error) {
	b, err := g.Bundle()
	if err != nil {
		return nil, err
	}
	return b.Dynamic, nil
}

func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "settings",
		Aliases: []string{"setting"},
		Short:   "Manage cluster-wide agent settings (security defaults wizard + apply).",
	}
	cmd.AddCommand(
		newSettingsApplyCmd(g),
		newSettingsWizardCmd(g),
	)
	return cmd
}
