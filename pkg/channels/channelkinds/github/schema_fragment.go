package github

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// SpiceDBSchemaFragment used to declare github_user here. It moved to the
// base scaffold (pkg/authz/spicedb/schema/schema.zed) because three
// independent fragments now name it — this kind's own session links below,
// the gh toolkit's repo roles, and the directory sync's writes — and only one
// may declare it; a second declaration is refused by the composer. `string`
// and `onepassword_group` moved for the identical reason.
//
// The kind still satisfies SchemaContributor — SessionRelationLinks below
// depends on github_user resolving in the composed schema, and the interface
// is where a future fragment contribution would go — but contributes no
// RawZed today.
func (k *Kind) SpiceDBSchemaFragment() *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{}
}

// SessionRelationLinks admits github_user#user as an agentsession
// owner/participant/denied subject type. This is what lets a
// pull-request-triggered session record its PR author as an owner BY GITHUB
// ACCOUNT: the tuple names the account (numeric id, same key the scaffold's
// github_user definition uses), and it resolves to an actual platform user
// only through the attested identity edge the useridentity reconciler mints
// from a verified credential.
// Until that person links a GitHub credential, the subject-set is empty and the
// tuple grants nobody anything; the moment they do, standing lights up with no
// rewrite here.
//
// This makes agentsession membership the FIRST authorization consumer of
// github_user — see the consumer allowlist test beside the attested-edge
// collision handling (pkg/authz/guardian/schema/github_user_inert_test.go),
// which pins that it stays the only one.
func (k *Kind) SessionRelationLinks() []string {
	return []string{"github_user#user"}
}

// Compile-time checks: *Kind satisfies the optional SchemaContributor and
// SessionRelationLinker interfaces.
var (
	_ channelkinds.SchemaContributor     = (*Kind)(nil)
	_ channelkinds.SessionRelationLinker = (*Kind)(nil)
)
