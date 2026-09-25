// Package installcmd implements the cluster-lifecycle commands: `oap install`,
// `oap init`, `oap build`, `oap image`, `oap check`, `oap clean`, `oap platform` and
// `oap spicedb`.
//
// These are eight separate top-level commands rather than one subtree, and
// they live together because they share the install's own decisions — which
// cluster kind, which registry, which storage classes, which routing — and a
// second copy of any of those answers is a way for two commands to install
// differently onto one cluster.
package installcmd
