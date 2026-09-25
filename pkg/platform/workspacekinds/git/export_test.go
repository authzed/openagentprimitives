package git

// GitEnvForTest exposes the package-private gitEnv to external tests. It lives
// in an _test.go file, so it never enters the production API surface.
var GitEnvForTest = gitEnv
