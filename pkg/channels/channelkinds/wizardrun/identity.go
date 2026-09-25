package wizardrun

import "github.com/authzed/openagentprimitives/pkg/channels/channelkinds"

// NormalizeOutputIdentity binds wizard-produced objects to the exact names an
// install graph planned. Wizards may derive conventional names locally, but a
// graph may truncate or digest-suffix each resource independently.
func NormalizeOutputIdentity(out *channelkinds.WizardOutput, namespace, channelName, credentialName, agentClass string) {
	if out == nil {
		return
	}
	if out.SecretManifest != nil {
		out.SecretManifest.APIVersion = "v1"
		out.SecretManifest.Kind = "Secret"
		out.SecretManifest.Name = credentialName
		out.SecretManifest.Namespace = namespace
	}
	if out.ChannelManifest != nil {
		out.ChannelManifest.APIVersion = "agentprimitives.authzed.com/v1alpha1"
		out.ChannelManifest.Kind = "Channel"
		out.ChannelManifest.Name = channelName
		out.ChannelManifest.Namespace = namespace
		out.ChannelManifest.Spec.AgentClass = agentClass
		out.ChannelManifest.Spec.CredentialsRef.SecretName = credentialName
	}
	if out.CapabilityPatch != nil {
		out.CapabilityPatch.SetName(agentClass)
		out.CapabilityPatch.SetNamespace(namespace)
	}
}
