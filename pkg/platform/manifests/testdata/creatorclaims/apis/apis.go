// Package apis is a FIXTURE for TestCreatorClaimsResolveStructurally: it stands
// in for pkg/apis/v1alpha1, the package whose constants cmd/oap names Secrets
// with instead of spelling them out. Parsed, never compiled.
package apis

const (
	// DemoImportedSecret is the shape of v1alpha1.PassthroughLinkSigningKeySecret.
	DemoImportedSecret = "demo-imported-secret"
	demoUnusedSecret   = "demo-unused-secret"
)
