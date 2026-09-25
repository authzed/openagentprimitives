package kube

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestSchemeHasGatewayTypes(t *testing.T) {
	assert.True(t, Scheme.Recognizes(gatewayv1.SchemeGroupVersion.WithKind("Gateway")), "Scheme must recognize Gateway")
	assert.True(t, Scheme.Recognizes(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute")), "Scheme must recognize HTTPRoute")
	assert.True(t, Scheme.Recognizes(gatewayv1.SchemeGroupVersion.WithKind("GatewayClass")), "Scheme must recognize GatewayClass")
}
