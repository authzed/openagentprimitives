package nats

import (
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateTLSServerCertVerifiesAgainstCA(t *testing.T) {
	m, err := GenerateTLS([]string{"spicebox-nats.agentprimitives-system.svc"})
	require.NoError(t, err, "GenerateTLS")

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(m.CACertPEM), "CA must parse")

	cert, err := parseLeaf(m.ServerCertPEM)
	require.NoError(t, err, "server cert must parse")

	_, err = cert.Verify(x509.VerifyOptions{
		DNSName: "spicebox-nats.agentprimitives-system.svc",
		Roots:   pool,
	})
	assert.NoError(t, err, "server cert must verify against the CA for its SAN")
}
