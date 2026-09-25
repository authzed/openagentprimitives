package main

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	webskill "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/skill"
)

// operatorServiceAccountName is the ServiceAccount the operator Deployment runs
// as, fixed by config/manager/serviceaccount.yaml. Combined with the operator's
// own namespace it forms the authenticated username the API server puts in
// AdmissionRequest.UserInfo for every write this process makes.
//
// A constant rather than a flag: it is not an operator's choice — it is what
// the install bundle creates, and a mismatch would silently disable the check
// it feeds rather than fail loudly, which is the wrong failure for something
// whose whole job is telling a materialization from a forgery.
const operatorServiceAccountName = "spicebox-operator"

// newSkillWebhook builds the Skill validating webhook with the operator's own
// identity, so a Skill claiming a git authority is admitted only when this
// process is the one writing it.
func newSkillWebhook(c client.Reader, d admission.Decoder, operatorUsername string) *webskill.Webhook {
	w := webskill.NewSkillWebhook(c, d)
	w.OperatorUsername = operatorUsername
	return w
}

// newClusterSkillWebhook is the ClusterSkill twin of newSkillWebhook.
func newClusterSkillWebhook(c client.Reader, d admission.Decoder, operatorUsername string) *webskill.ClusterSkillWebhook {
	w := webskill.NewClusterSkillWebhook(c, d)
	w.OperatorUsername = operatorUsername
	return w
}
