// Package clinats loads the CLI's NATS credentials from the cluster and
// materializes them as on-disk files for apnats.Connect.
//
// `oap install` creates the Secret spicebox-cli-nats-creds in
// agentprimitives-system with data keys `nats.creds` (a decorated NATS
// user creds file) and `ca.crt` (the PEM CA the NATS server cert is
// verified against). apnats.Connect wants filesystem paths, not bytes,
// so we write each value to a temp file and hand back the paths plus a
// cleanup func the caller defers.
package clinats

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
)

const cliNATSCredsSecret = "spicebox-cli-nats-creds"

// LoadCreds reads the spicebox-cli-nats-creds Secret, writes its
// `nats.creds` and `ca.crt` values to temp files, and returns an
// apnats.Options populated with CredsPath / CAPath / ServerName (URL is
// left for the caller — it differs per command, e.g. a port-forwarded
// 127.0.0.1 address).
//
// On error, the returned cleanup func is nil and Options is zero; callers
// MUST check the error before deferring cleanup. On success, callers MUST
// defer the cleanup func to remove the temp files.
//
// If the Secret is missing — an older cluster installed before NATS
// auth — the error tells the user to re-run `oap install`. We never
// silently fall back to an unauthenticated connect: the NATS server now
// requires creds, so a plain connect would fail anyway, and a clear
// error beats an opaque connection refusal.
func LoadCreds(ctx context.Context, b *kube.Bundle) (apnats.Options, func(), error) {
	var sec corev1.Secret
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: "agentprimitives-system", Name: cliNATSCredsSecret,
	}, &sec); err != nil {
		return apnats.Options{}, nil, fmt.Errorf(
			"read NATS creds Secret agentprimitives-system/%s: %w "+
				"(re-run `oap install` — this cluster predates NATS authentication)",
			cliNATSCredsSecret, err)
	}

	creds := sec.Data["nats.creds"]
	if len(creds) == 0 {
		return apnats.Options{}, nil, fmt.Errorf(
			"NATS creds Secret %s has empty nats.creds key — re-run `oap install`", cliNATSCredsSecret)
	}
	ca := sec.Data["ca.crt"]
	if len(ca) == 0 {
		return apnats.Options{}, nil, fmt.Errorf(
			"NATS creds Secret %s has empty ca.crt key — re-run `oap install`", cliNATSCredsSecret)
	}

	credsPath, err := writeTempCreds("ap-nats-*.creds", creds)
	if err != nil {
		return apnats.Options{}, nil, err
	}
	caPath, err := writeTempCreds("ap-nats-ca-*.crt", ca)
	if err != nil {
		// Don't leak the creds temp file if the second write failed.
		_ = os.Remove(credsPath)
		return apnats.Options{}, nil, err
	}

	cleanup := func() {
		if err := os.Remove(credsPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "ap: failed to remove temp creds file %s: %v\n", credsPath, err)
		}
		if err := os.Remove(caPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "ap: failed to remove temp CA file %s: %v\n", caPath, err)
		}
	}

	return apnats.Options{
		CredsPath:  credsPath,
		CAPath:     caPath,
		ServerName: apnats.ServerName,
	}, cleanup, nil
}

// writeTempCreds writes data to a new temp file created from pattern and
// returns its path. The file is created 0600 (os.CreateTemp default).
func writeTempCreds(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("create temp file %s: %w", pattern, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write temp file %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close temp file %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}
