package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

const (
	spiceDBTokenSecretName  = "spicebox-spicedb-token"
	postgresTokenSecretName = "spicebox-postgres-token"
)

// MintOptions tunes which optional, instance-coupled Secrets MintAllInternalSecrets
// creates. Both default to false (mint); set true to skip when the user points at
// an EXTERNAL instance and provides the Secret themselves.
type MintOptions struct {
	SkipSpiceDBToken  bool // external SpiceDB: token is the user's pre-shared key
	SkipPostgresToken bool // external Postgres: URI is the user's
}

// MintAllInternalSecrets creates every app-internal Secret the bundle needs
// (except webhook-tls, which is provisioned separately), idempotently and
// without rotating anything that already exists. It is the entry point the
// pre-start bootstrap Job runs, so every consumer mounts its Secret cleanly at
// first boot. reader must be a live/uncached reader; writer the controller client.
//
// NOTE: the NATS Secrets (identity/tls/creds) are minted by MintNATSSecrets,
// added in a follow-up commit; this covers the token-shaped ones.
func MintAllInternalSecrets(ctx context.Context, reader client.Reader, writer client.Client, opts MintOptions) error {
	for _, name := range append(append([]string{}, MemoryTokenSecretNames...), AdmindTokenSecretName) {
		if _, err := EnsureTokenSecret(ctx, reader, writer, TokenSecretNamespace, name); err != nil {
			return err
		}
	}
	if _, err := EnsurePassthroughLinkKey(ctx, reader, writer, nil); err != nil {
		return err
	}
	// NATS identity → TLS → client creds (creds require the first two). Always
	// bundled; never gated by the external flags.
	if err := EnsureNATSIdentity(ctx, writer); err != nil {
		return err
	}
	if err := EnsureNATSTLS(ctx, writer); err != nil {
		return err
	}
	if err := EnsureNATSClientCreds(ctx, writer, nil); err != nil {
		return err
	}
	if !opts.SkipPostgresToken {
		if err := EnsurePostgresToken(ctx, reader, writer); err != nil {
			return err
		}
	}
	if !opts.SkipSpiceDBToken {
		if err := EnsureSpiceDBToken(ctx, reader, writer); err != nil {
			return err
		}
		// The /spicedb datastore URI is derived from the postgres password, so it
		// can only be set once BOTH tokens exist. Skipped when either is external.
		if !opts.SkipPostgresToken {
			if err := EnsureSpiceDBDatastoreURI(ctx, reader, writer); err != nil {
				return err
			}
		}
	}
	return nil
}

// EnsureSpiceDBToken creates spicebox-spicedb-token (32 random bytes hex) with
// the dual `token`/`preshared_key` shape, reconciling an ESO/GitOps-provisioned
// preshared_key-only Secret. Create-if-absent; never rotates an existing token.
func EnsureSpiceDBToken(ctx context.Context, reader client.Reader, writer client.Client) error {
	var existing corev1.Secret
	err := reader.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: spiceDBTokenSecretName}, &existing)
	if err == nil {
		tok, pre := existing.Data["token"], existing.Data["preshared_key"]
		if len(tok) > 0 && bytes.Equal(tok, pre) {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		switch {
		case len(tok) > 0:
			existing.Data["preshared_key"] = tok
		case len(pre) > 0:
			existing.Data["token"] = pre
		default:
			minted, mErr := randomHexKey()
			if mErr != nil {
				return mErr
			}
			existing.Data["token"], existing.Data["preshared_key"] = minted, minted
		}
		if err := writer.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update spicedb token secret: %w", err)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get spicedb token secret: %w", err)
	}
	tok, err := randomHexKey()
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: spiceDBTokenSecretName, Namespace: TokenSecretNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": tok, "preshared_key": tok},
	}
	adoptguard.WithAdoptedLabel(sec)
	if err := writer.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create spicedb token secret: %w", err)
	}
	return nil
}

// EnsurePostgresToken creates spicebox-postgres-token (16 random bytes hex
// password + a `uri` to the bundled Postgres `/memory` DB). Create-if-absent;
// never rotates.
func EnsurePostgresToken(ctx context.Context, reader client.Reader, writer client.Client) error {
	var existing corev1.Secret
	err := reader.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: postgresTokenSecretName}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get postgres token secret: %w", err)
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate postgres password: %w", err)
	}
	password := hex.EncodeToString(buf)
	uri := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", password),
		Host:     "spicebox-postgres.agentprimitives-system.svc:5432",
		Path:     "/memory",
		RawQuery: "sslmode=disable",
	}).String()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: postgresTokenSecretName, Namespace: TokenSecretNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"password": []byte(password), "uri": []byte(uri)},
	}
	adoptguard.WithAdoptedLabel(sec)
	if err := writer.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create postgres token secret: %w", err)
	}
	return nil
}

// EnsureSpiceDBDatastoreURI writes a `datastore_uri` (the bundled Postgres
// `/spicedb` DB, password read from spicebox-postgres-token) onto
// spicebox-spicedb-token. Both Secrets must already exist. Idempotent.
func EnsureSpiceDBDatastoreURI(ctx context.Context, reader client.Reader, writer client.Client) error {
	var pg corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: postgresTokenSecretName}, &pg); err != nil {
		return fmt.Errorf("get postgres token secret: %w", err)
	}
	pw := string(pg.Data["password"])
	if pw == "" {
		return fmt.Errorf("postgres token secret has no password")
	}
	uri := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", pw),
		Host:     "spicebox-postgres.agentprimitives-system.svc:5432",
		Path:     "/spicedb",
		RawQuery: "sslmode=disable",
	}).String()
	var sp corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: TokenSecretNamespace, Name: spiceDBTokenSecretName}, &sp); err != nil {
		return fmt.Errorf("get spicedb token secret: %w", err)
	}
	if string(sp.Data["datastore_uri"]) == uri {
		return nil
	}
	if sp.Data == nil {
		sp.Data = map[string][]byte{}
	}
	sp.Data["datastore_uri"] = []byte(uri)
	if err := writer.Update(ctx, &sp); err != nil {
		return fmt.Errorf("update spicedb datastore_uri: %w", err)
	}
	return nil
}
