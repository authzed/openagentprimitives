package apcmd

// The cluster-side names `oap install` writes and every other command has to
// look under. They are shared rather than re-spelled per command because a
// command reading the wrong namespace does not fail — it reports "not
// installed" about a cluster that is.
const (
	// SystemNamespace holds the platform's own workloads: the operator, webd,
	// channelsd, SpiceDB, NATS, and the Secrets they are configured from.
	SystemNamespace = "agentprimitives-system"

	// OperatorDeployment is the operator's Deployment inside SystemNamespace.
	// Several commands read its image to learn which registry an install came
	// from.
	OperatorDeployment = "spicebox-operator"
)
