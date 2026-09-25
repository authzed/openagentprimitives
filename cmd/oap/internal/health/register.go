package health

// The first-party component set `oap check` verifies. Registration order is the
// reported order. Add a new component here (or call Register from its own file)
// rather than editing the check command.
func init() {
	// Core required components — a not-ready state FAILS `oap check`.
	Register(Deployment("spicebox-operator", false))
	Register(Deployment("agentprimitives-authzd", false))
	Register(Deployment("spicebox-channelsd", false))
	// spicedb is operator-managed: the spicedb-operator creates the Deployment
	// from the "spicebox-spicedb" SpiceDBCluster CR and names it "<cluster>-spicedb",
	// hence the doubled "spicebox-spicedb-spicedb". (The Service stays "spicebox-spicedb".)
	Register(Deployment("spicebox-spicedb-spicedb", false))
	Register(Deployment("spicebox-webd", false))
	Register(Deployment("spicebox-postgres", false))
	Register(StatefulSet("spicebox-nats", false))
	Register(StatefulSet("spicebox-neo4j", false))

	// Graphiti (knowledge-graph entity extraction) is OPTIONAL: it needs
	// OPENAI_API_KEY and the core stack functions without it, so a not-ready
	// state WARNS rather than failing — mirroring the install, which never fails
	// on a not-ready graphiti.
	Register(Deployment("spicebox-graphiti", true))
}
