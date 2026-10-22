package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={authzed,spicebox},shortName=wha
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Channel",type="string",JSONPath=".spec.channelName"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.recordType"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.reason",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//
// WebhookAdmission durably records a verified webhook delivery or an atomic
// provider-cohort claim before creating an AgentSession. It contains no secret
// credential. Provider admission workers update only status; the delivery body
// and Channel incarnation are immutable evidence.
type WebhookAdmission struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WebhookAdmissionSpec   `json:"spec"`
	Status            WebhookAdmissionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WebhookAdmissionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebhookAdmission `json:"items"`
}

// WebhookAdmissionSpec is immutable after creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="webhook admission evidence is immutable"
type WebhookAdmissionSpec struct {
	// RecordType separates raw deliveries from deduplicated CI-cohort claims.
	// +kubebuilder:validation:Enum=delivery;cohort
	RecordType string `json:"recordType"`
	// ChannelName names the input Channel in this namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ChannelName string `json:"channelName"`
	// ChannelUID pins the Channel incarnation across deletion and recreation.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ChannelUID types.UID `json:"channelUID"`
	// Event is the verified provider event name.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Event string `json:"event,omitempty"`
	// Body is the authenticated webhook payload, never an installation token.
	// Keep the wire encoding as a plain base64 string: Kubernetes 1.30 CEL
	// byte conversion rejects the standard +/ alphabet in immutable-spec checks.
	// The Go JSON decoder still enforces base64 and preserves exact bytes.
	// MaxLength bounds the base64 representation of a 512 KiB delivery.
	// +kubebuilder:validation:Format=""
	// +kubebuilder:validation:MaxLength=699052
	// +optional
	Body []byte `json:"body,omitempty"`
	// EnqueuedAt starts the bounded retention period independently of retries.
	EnqueuedAt metav1.Time `json:"enqueuedAt"`
}

// WebhookAdmissionStatus holds the durable worker lease and admission result.
type WebhookAdmissionStatus struct {
	// Phase is pending until the delivery or cohort was processed conclusively.
	// +kubebuilder:validation:Enum=pending;done
	// +optional
	Phase string `json:"phase,omitempty"`
	// Reason records deferral, expiry, provider failure, or supersession evidence.
	// +optional
	Reason string `json:"reason,omitempty"`
	// RetryCount counts consecutive transient failures for bounded retry backoff.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`
	// LeaseUntil serializes workers using status resource-version comparisons
	// and acts as retry-not-before after a transient provider or delivery error.
	// +optional
	LeaseUntil metav1.Time `json:"leaseUntil,omitempty"`
}

func init() { SchemeBuilder.Register(&WebhookAdmission{}, &WebhookAdmissionList{}) }
