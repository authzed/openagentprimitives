// Package assetlimits holds size ceilings shared by both sides of the
// inbound-attachment upload path: pkg/memory/httpsrv (the operator's POST
// /inbound-asset route, which enforces the ceiling) and
// pkg/channels/channelsd/pipeline (which clamps its own pre-fetch size check to
// the same number, so a file the operator will reject is never downloaded in
// full first). It exists ONLY to let the two import one constant without either
// importing the other.
//
// It deliberately has NO dependencies — not even pkg/memory — and must stay
// that way. pipeline is reachable from the browser-facing pkg/web/webui/chat
// (via pipelinehost), and pkg/memory/httpsrv pulls in pkg/memory/provenance (the
// audit-signing package), so pipeline importing httpsrv just to read one integer
// hands the browser-facing package a transitive import of the signing path.
// TestChatPackageNeverImportsProvenance (pkg/web/webui/chat/session_test.go)
// fails on that edge. Do not "simplify" this back into httpsrv, and do not add
// an import here without first checking it does not reintroduce the same
// problem one hop removed.
package assetlimits

// MaxInboundAssetBytes hard-caps a single inbound attachment upload: the
// operator's absolute ceiling for the non-native (extracted) path, independent
// of whatever a Channel's spec.attachments.maxSizeBytes says.
// pkg/memory/httpsrv.MaxInboundAssetBytes aliases it; pipeline imports THIS
// package directly.
const MaxInboundAssetBytes = 25 << 20 // 25 MiB
