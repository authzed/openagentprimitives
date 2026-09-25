// Package creators is a FIXTURE for TestCreatorClaimsResolveStructurally, not
// production code: it stands in for cmd/oap's ensure* helpers so the resolver
// behind installTimeSecrets can be tested against every shape of claim,
// including the false one. It is parsed, never compiled (the go tool ignores
// testdata), so an unresolved import path is fine.
package creators

// localSecretName is the same-package constant shape.
const localSecretName = "demo-local-secret"

// ensureCommentOnly is the FICTION case, and the one that shipped: it names
// demo-comment-secret in prose while creating something else entirely. A raw
// text grep over the concatenated package cannot tell this apart from a real
// claim.
func ensureCommentOnly() string {
	return "demo-unrelated-secret"
}

func ensureLiteral() string {
	return "demo-literal-secret"
}

// ensureViaLocalConst names its Secret through a same-package constant.
func ensureViaLocalConst() string {
	return localSecretName
}

// ensureViaImportedConst names its Secret through another package's constant,
// the way cmd/oap reaches for v1alpha1.PassthroughLinkSigningKeySecret.
func ensureViaImportedConst() string {
	return apis.DemoImportedSecret
}

// ensureDelegating does the work one hop down, as cmd/oap's ensureSpiceDBToken
// and ensureNATSClientCreds both do.
func ensureDelegating() string {
	return ensureDelegatingWithClient()
}

func ensureDelegatingWithClient() string {
	return "demo-delegated-secret"
}
