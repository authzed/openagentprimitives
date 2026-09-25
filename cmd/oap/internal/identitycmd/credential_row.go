package identitycmd

import (
	"fmt"
	"io"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// credRow is one credential's presentation row. `identity show` and
// `useridentity show` print the exact same shape for an AgentCredential, so
// both route through this one type + credentialRow rather than each carrying
// its own copy of the switch this used to be.
type credRow struct {
	// Name is the credential's name within its owning identity CR.
	Name string
	// TypeLabel is the human-facing label: the credkind's DisplayName(), or
	// "unknown (<type>)" when the type isn't registered in this build. Never
	// blank — see credentialRow's doc for why that matters.
	TypeLabel string
	// SecretName is the backing Secret's name, or "" when the type has no
	// backing Secret (a minted type) or the type is unrecognized.
	SecretName string
	// SecretKey is the key within that Secret, or "" for a type whose Secret
	// has a fixed multi-key shape (oauth) rather than one named key.
	SecretKey string
}

// credentialRow projects one AgentCredential into its presentation row via
// the credkind registry.
//
// An unregistered type degrades VISIBLY rather than vanishing into a blank
// line: TypeLabel reads "unknown (<type>)" so an operator scanning `oap
// identity show` (or `oap useridentity show`) output can tell the credential
// is there and simply unrecognized by this build — not silently dropped.
func credentialRow(c spiceboxv1alpha1.AgentCredential) credRow {
	k, err := credkindregistry.Get(c.Type)
	if err != nil {
		return credRow{Name: c.Name, TypeLabel: fmt.Sprintf("unknown (%s)", c.Type)}
	}
	row := credRow{Name: c.Name, TypeLabel: k.DisplayName()}
	if ref := k.SecretRef(c); ref != nil {
		row.SecretName, row.SecretKey = ref.Name, ref.Key
	}
	return row
}

// printCredentialRow renders one credRow in the shape both `identity show`
// and `useridentity show` use: the credential name, its type label, and —
// when it has one — the backing Secret (and key, for a single-key type like
// static; a type like oauth whose Secret has a fixed multi-key shape prints
// just the Secret name).
func printCredentialRow(out io.Writer, row credRow) {
	switch {
	case row.SecretKey != "":
		fmt.Fprintf(out, "  - %s (%s; secret=%s key=%s)\n", row.Name, row.TypeLabel, row.SecretName, row.SecretKey)
	case row.SecretName != "":
		fmt.Fprintf(out, "  - %s (%s; secret=%s)\n", row.Name, row.TypeLabel, row.SecretName)
	default:
		fmt.Fprintf(out, "  - %s (%s)\n", row.Name, row.TypeLabel)
	}
}
