// pkg/controllers/agentclass/schema_validation_test.go
//
// Pure-function tests for validateAgainstSchema — no envtest, no live
// SpiceDB. Uses a fake SchemaReader that returns a static schema text
// so the test can pin the slice-4 invariant that PermissionVariants
// references are validated alongside the singular Permission.
package agentclass

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// fakeSchemaReader returns a static schema text on ReadSchema, or a
// fixed error when readErr is set (simulating a transient SpiceDB blip).
type fakeSchemaReader struct {
	text    string
	readErr error
}

func (f *fakeSchemaReader) ReadSchema(_ context.Context, _ *v1.ReadSchemaRequest) (*v1.ReadSchemaResponse, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return &v1.ReadSchemaResponse{SchemaText: f.text}, nil
}

const slice4SchemaText = `
definition user {}
definition github_repo {
    relation reader: user
    permission read = reader
}
definition crm_company {
    relation grant_contact_access: user
    permission contact_access = grant_contact_access
}
`

// TestValidateAgainstSchema_VariantsValidated asserts the slice-4
// invariant: a PermissionVariant referencing a typo'd resource or
// permission fails validation with ReasonPermissionSchemaMismatch,
// even when the singular Permission would resolve cleanly.
func TestValidateAgainstSchema_VariantInvalid(t *testing.T) {
	r := &fakeSchemaReader{text: slice4SchemaText}
	servers := []spiceboxv1alpha1.MCPServer{
		{
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Tools: []spiceboxv1alpha1.MCPServerTool{
					{
						Name: "search_crm_objects",
						// Fallback Permission resolves cleanly.
						Permission: &authz.Permission{
							StateImpact: authz.Readonly,
							Check: &authz.PermissionCheck{
								ResourceType: "github_repo",
								Permission:   "read",
							},
						},
						PermissionVariants: []authz.PermissionVariant{
							{
								When: `args.objectType == "contacts"`,
								Check: authz.Permission{
									StateImpact: authz.Readonly,
									Check: &authz.PermissionCheck{
										// Typo: schema declares contact_access, not contacts_access.
										ResourceType: "crm_company",
										Permission:   "contacts_access",
									},
								},
							},
						},
					},
				},
			},
		},
	}
	reason, msg, err := validateAgainstSchema(context.Background(), r, servers)
	require.NoError(t, err, "a schema mismatch is not a transient error")
	require.Equal(t, spiceboxv1alpha1.ReasonPermissionSchemaMismatch, reason,
		"reason for variant-typo invariant; msg=%q", msg)
	assert.Contains(t, msg, "permissionVariants[0]",
		"error message should reference permissionVariants[0]")
	assert.Contains(t, msg, "contacts_access",
		"error message should name the offending permission")
	assert.Contains(t, msg, "crm_company",
		"error message should name the offending resource")
}

// TestValidateAgainstSchema_VariantValid asserts that a well-formed
// PermissionVariant referencing a real (resourceType, permission) pair
// passes validation.
func TestValidateAgainstSchema_VariantValid(t *testing.T) {
	r := &fakeSchemaReader{text: slice4SchemaText}
	servers := []spiceboxv1alpha1.MCPServer{
		{
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Tools: []spiceboxv1alpha1.MCPServerTool{
					{
						Name: "search_crm_objects",
						Permission: &authz.Permission{
							StateImpact: authz.Passthrough,
						},
						PermissionVariants: []authz.PermissionVariant{
							{
								When: `args.objectType == "contacts"`,
								Check: authz.Permission{
									StateImpact: authz.Readonly,
									Check: &authz.PermissionCheck{
										ResourceType: "crm_company",
										Permission:   "contact_access",
									},
								},
							},
							{
								When: `args.objectType == "companies"`,
								Check: authz.Permission{
									StateImpact: authz.Passthrough,
								},
							},
						},
					},
				},
			},
		},
	}
	reason, msg, err := validateAgainstSchema(context.Background(), r, servers)
	require.NoError(t, err, "clean validation must not error")
	assert.Empty(t, reason, "expected clean validation; msg=%q", msg)
}

// TestValidateAgainstSchema_TransientVsMismatch pins the audit fix: a
// transient SpiceDB-unreachable / fetch failure is surfaced as a non-nil
// error (so the controller requeues) WITHOUT a Valid=False reason, while a
// genuine schema mismatch is surfaced as a reason with NO error (so the
// controller parks Valid=False without requeue). Treating the two the same
// was what permanently parked the class on a momentary SpiceDB blip.
func TestValidateAgainstSchema_TransientVsMismatch(t *testing.T) {
	// One tool whose Check references a (resourceType, permission) pair the
	// schema below does NOT declare — the genuine-mismatch case.
	mismatchServers := []spiceboxv1alpha1.MCPServer{
		{
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Tools: []spiceboxv1alpha1.MCPServerTool{
					{
						Name: "search_crm_objects",
						Permission: &authz.Permission{
							StateImpact: authz.Readonly,
							Check: &authz.PermissionCheck{
								ResourceType: "github_repo",
								Permission:   "no_such_permission",
							},
						},
					},
				},
			},
		},
	}

	cases := []struct {
		name           string
		reader         spicedb.SchemaReader
		servers        []spiceboxv1alpha1.MCPServer
		wantErr        bool   // transient → controller requeues
		wantReason     string // mismatch → controller parks Valid=False (no requeue)
		wantReasonText string // substring expected on the error/reason for context
	}{
		{
			name:           "SpiceDB fetch fails: transient error returned (controller requeues), no Valid=False reason",
			reader:         &fakeSchemaReader{readErr: errors.New("connection refused")},
			servers:        mismatchServers,
			wantErr:        true,
			wantReasonText: spiceboxv1alpha1.ReasonSpiceDBUnreachable,
		},
		{
			name:           "schema text unparseable: transient error returned (controller requeues), no Valid=False reason",
			reader:         &fakeSchemaReader{text: "this is not valid spicedb dsl {{{"},
			servers:        mismatchServers,
			wantErr:        true,
			wantReasonText: spiceboxv1alpha1.ReasonSpiceDBUnreachable,
		},
		{
			name:           "genuine schema mismatch: Valid=False reason returned (no requeue), no error",
			reader:         &fakeSchemaReader{text: slice4SchemaText},
			servers:        mismatchServers,
			wantErr:        false,
			wantReason:     spiceboxv1alpha1.ReasonPermissionSchemaMismatch,
			wantReasonText: "no_such_permission",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg, err := validateAgainstSchema(context.Background(), tc.reader, tc.servers)
			if tc.wantErr {
				require.Error(t, err, "transient failure must return an error so the controller requeues")
				assert.Empty(t, reason, "transient failure must NOT set a Valid=False reason; reason=%q", reason)
				assert.Empty(t, msg, "transient failure carries its detail on the error, not the message")
				assert.Contains(t, err.Error(), tc.wantReasonText,
					"transient error should name the SpiceDB-unreachable cause")
				return
			}
			require.NoError(t, err, "a schema mismatch is a spec property, not a transient error")
			assert.Equal(t, tc.wantReason, reason, "mismatch reason; msg=%q", msg)
			assert.Contains(t, msg, tc.wantReasonText, "mismatch message should name the offending permission")
		})
	}
}
