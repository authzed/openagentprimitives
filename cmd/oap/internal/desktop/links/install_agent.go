package links

// KeyInstallAgent installs an agent bundle from an OCI reference.
const KeyInstallAgent = "install/agent"

func init() {
	Register(Spec{Key: KeyInstallAgent, MaxVersion: 1, Params: []string{"ref"}})
}
