package oci

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/payload"

	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

// Cosign-format constants and shapes, matched byte-for-byte against cosign v2's
// OCI-1.1-referrers signing path, so a .oap signed by Sign verifies with
// `cosign verify --key ... --registry-referrers-mode=oci-1-1` and vice versa.
// Cosign itself is NOT a dependency of this module — go.mod/go.sum are untouched
// by it; only its source was read to confirm interop. The payload JSON shape
// comes from github.com/sigstore/sigstore's signature/payload package, which IS
// a real dependency and is the same package cosign's CLI imports for this.
const (
	// cosignSimpleSigningMediaType is the payload blob's media type — cosign's
	// "Simple Signing" envelope (github.com/sigstore/cosign/v2/pkg/types.SimpleSigningMediaType).
	cosignSimpleSigningMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"

	// cosignSignatureAnnotationKey carries base64(signature) on the payload
	// layer's descriptor within the signature manifest
	// (github.com/sigstore/cosign/v2/pkg/oci/static.SignatureAnnotationKey).
	cosignSignatureAnnotationKey = "dev.cosignproject.cosign/signature"

	// cosignSigArtifactType is cosign's referrers-mode artifactType for an image
	// signature (as opposed to an attestation or SBOM referrer). Cosign encodes
	// it in the signature manifest's config.mediaType rather than the manifest's
	// top-level `artifactType`, and both the OCI-distribution backward-compat
	// rule and the in-process test registry key off config.mediaType, ignoring
	// any top-level field. Matched exactly (config.mediaType only) for interop
	// with real cosign and with that test registry alike.
	cosignSigArtifactType = "application/vnd.dev.cosign.artifact.sig.v1+json"
)

// cosignSigConfigBytes is the config blob for the signature manifest, unparsed
// and ignored on verify. Neither cosign's verify path nor ours inspects its
// content — only its mediaType, the artifactType signal above — so a minimal
// `{}` suffices, matching the minimal-empty-config pattern cosign uses for its
// other referrer kinds.
var cosignSigConfigBytes = []byte("{}")

// ErrNoValidSignature is returned by Verify when ref has no cosign signature
// referrer that both validates cryptographically against pub and carries a
// simple-signing payload naming ref's current manifest digest. Verify fails
// closed: no referrers, a wrong key, and a tampered/re-pushed manifest all
// collapse to this one error rather than a silent partial pass.
var ErrNoValidSignature = errors.New("oap/oci: no valid cosign signature found")

// Sign resolves ref's current manifest descriptor (the "subject"), builds a
// cosign-format simple-signing payload naming that digest, signs the payload
// with signer (an ECDSA-P256 key is expected, per cosign's supported scheme),
// and pushes the result as an OCI referrer of ref (the pushed signature
// manifest's `subject` field points at ref's manifest descriptor). It
// returns the digest of the pushed signature manifest.
func Sign(ctx context.Context, ref string, signer crypto.Signer, opts Options) (string, error) {
	repo, err := repository(ref, opts)
	if err != nil {
		return "", err
	}

	subject, err := repo.Resolve(ctx, repo.Reference.ReferenceOrDefault())
	if err != nil {
		return "", fmt.Errorf("sign %s: resolve subject manifest: %w", ref, err)
	}

	// signature.LoadSignerVerifier dispatches on signer's concrete type
	// (crypto.PrivateKey is an alias for `any`; a *ecdsa.PrivateKey boxed as
	// crypto.Signer still type-switches to the ECDSA case).
	sv, err := signature.LoadSignerVerifier(signer, crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("sign %s: load signer: %w", ref, err)
	}

	repoName := repo.Reference.Registry + "/" + repo.Reference.Repository
	imgDigest, err := name.NewDigest(repoName + "@" + subject.Digest.String())
	if err != nil {
		return "", fmt.Errorf("sign %s: build image digest reference: %w", ref, err)
	}

	// payload.Cosign{...}.MarshalJSON produces cosign's exact "simple signing"
	// envelope. The docker-reference value is ignored by cosign's own verifier
	// (per its SIGNATURE_SPEC.md), so leaving ClaimedIdentity unset — defaulting
	// to the repository name — is fine.
	payloadBytes, err := (payload.Cosign{Image: imgDigest}).MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("sign %s: marshal simple-signing payload: %w", ref, err)
	}

	// SignMessage hashes payloadBytes with SHA-256 and produces an ASN.1 DER
	// ECDSA signature (crypto/ecdsa.SignASN1 under the hood) — the same shape
	// cosign's own signer produces and its verifier accepts.
	sig, err := sv.SignMessage(bytes.NewReader(payloadBytes))
	if err != nil {
		return "", fmt.Errorf("sign %s: sign payload: %w", ref, err)
	}
	b64sig := base64.StdEncoding.EncodeToString(sig)

	payloadDesc := content.NewDescriptorFromBytes(cosignSimpleSigningMediaType, payloadBytes)
	if err := pushBlob(ctx, repo.Blobs(), payloadDesc, payloadBytes); err != nil {
		return "", fmt.Errorf("sign %s: push signature payload: %w", ref, err)
	}
	// The signature annotation lives on the *reference* to the payload blob
	// inside the manifest (the layer entry), not on the blob's own push
	// descriptor — set it after pushing so pushBlob's descriptor stays a pure
	// content-addressed identity.
	payloadDesc.Annotations = map[string]string{cosignSignatureAnnotationKey: b64sig}

	configDesc := content.NewDescriptorFromBytes(cosignSigArtifactType, cosignSigConfigBytes)
	if err := pushBlob(ctx, repo.Blobs(), configDesc, cosignSigConfigBytes); err != nil {
		return "", fmt.Errorf("sign %s: push signature config: %w", ref, err)
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{payloadDesc},
		Subject:   &subject,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("sign %s: marshal signature manifest: %w", ref, err)
	}
	manifestDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifestBytes)

	// Manifests().Push (not PushReference) pushes by digest and — via oras-go's
	// manifestStore.pushWithIndexing — automatically registers the manifest as a
	// referrer of subject: natively through the OCI 1.1 Referrers API where the
	// registry supports it, or via the client-side referrers-tag-schema fallback
	// index otherwise. Setting manifest.Subject above is all the linking needed.
	if err := repo.Manifests().Push(ctx, manifestDesc, bytes.NewReader(manifestBytes)); err != nil {
		return "", fmt.Errorf("sign %s: push signature manifest: %w", ref, err)
	}
	return manifestDesc.Digest.String(), nil
}

// pushBlob pushes data to pusher, tolerating a concurrent or prior push of
// the same content (content-addressed storage: identical digest means
// identical bytes already stored, so ErrAlreadyExists is not a failure).
func pushBlob(ctx context.Context, pusher content.Pusher, desc ocispec.Descriptor, data []byte) error {
	if err := pusher.Push(ctx, desc, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return nil
}

// Verify resolves ref's current manifest descriptor (the "subject"), fetches
// its OCI referrers, and looks for a cosign-format simple-signing referrer
// whose signature validates against pub and whose payload names that same
// digest. On success it returns the verified subject digest, so callers can pin
// subsequent fetches to it and close the tag-mutation TOCTOU gap. If no referrer
// satisfies both checks — no referrers, a non-matching key, or ref re-pushed
// with different content since it was signed — it returns ("",
// ErrNoValidSignature), wrapped with a short reason from the candidate that got
// furthest. It never reports a partial or best-effort success.
//
// Trust scope — DIGEST binding only. Verify checks that a valid signature exists
// over the subject manifest's digest, matching cosign's default ClaimVerifier.
// It does NOT verify identity/reference binding: the payload's docker-reference
// is not compared against ref (cosign's simple-signing spec declares that field
// ignored). A signed manifest and its signature referrer copied verbatim into a
// different repository therefore still verify with the original key. This is
// intentional parity with cosign, but callers MUST NOT read "Verify returned
// nil" as "this artifact is hosted where its signer intended" — only as "some
// holder of the private key signed exactly these manifest bytes."
func Verify(ctx context.Context, ref string, pub crypto.PublicKey, opts Options) (string, error) {
	repo, err := repository(ref, opts)
	if err != nil {
		return "", err
	}

	subject, err := repo.Resolve(ctx, repo.Reference.ReferenceOrDefault())
	if err != nil {
		return "", fmt.Errorf("verify %s: resolve subject manifest: %w", ref, err)
	}

	// signature.LoadVerifier dispatches on pub's concrete type the same way
	// LoadSignerVerifier does for signers.
	verifier, err := signature.LoadVerifier(pub, crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("verify %s: load verifier: %w", ref, err)
	}

	var referrers []ocispec.Descriptor
	if err := repo.Referrers(ctx, subject, cosignSigArtifactType, func(rs []ocispec.Descriptor) error {
		referrers = append(referrers, rs...)
		return nil
	}); err != nil {
		return "", fmt.Errorf("verify %s: list referrers: %w", ref, err)
	}

	// lastErr records why the furthest-progressing candidate failed, so a
	// failed Verify carries an actionable reason (bad base64 / crypto fail /
	// digest mismatch / …) rather than only the generic sentinel. It never
	// affects the pass/fail decision — a return nil below is reached only when
	// a candidate satisfies every check.
	var lastErr error
	subjectDigest := subject.Digest.String()
	for _, rdesc := range referrers {
		manifestBytes, err := content.FetchAll(ctx, repo, rdesc)
		if err != nil {
			lastErr = fmt.Errorf("fetch referrer manifest %s: %w", rdesc.Digest, err)
			continue // unreadable referrer: not a usable signature, try the next
		}
		var manifest ocispec.Manifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			lastErr = fmt.Errorf("decode referrer manifest %s: %w", rdesc.Digest, err)
			continue
		}
		for _, layer := range manifest.Layers {
			if layer.MediaType != cosignSimpleSigningMediaType {
				continue // not a cosign payload layer; silent, not a failure
			}
			b64sig, ok := layer.Annotations[cosignSignatureAnnotationKey]
			if !ok {
				lastErr = fmt.Errorf("payload layer %s missing %s annotation", layer.Digest, cosignSignatureAnnotationKey)
				continue
			}
			sigBytes, err := base64.StdEncoding.DecodeString(b64sig)
			if err != nil {
				lastErr = fmt.Errorf("decode signature annotation on %s: %w", layer.Digest, err)
				continue
			}
			payloadBytes, err := content.FetchAll(ctx, repo, layer)
			if err != nil {
				lastErr = fmt.Errorf("fetch signature payload %s: %w", layer.Digest, err)
				continue
			}
			if err := verifier.VerifySignature(bytes.NewReader(sigBytes), bytes.NewReader(payloadBytes)); err != nil {
				lastErr = fmt.Errorf("signature over payload %s failed to verify (wrong key?): %w", layer.Digest, err)
				continue // cryptographically invalid — e.g. the wrong public key
			}
			var simple payload.SimpleContainerImage
			if err := json.Unmarshal(payloadBytes, &simple); err != nil {
				lastErr = fmt.Errorf("decode simple-signing payload %s: %w", layer.Digest, err)
				continue
			}
			if simple.Critical.Type != payload.CosignSignatureType {
				lastErr = fmt.Errorf("payload %s has unexpected critical.type %q", layer.Digest, simple.Critical.Type)
				continue
			}
			if simple.Critical.Image.DockerManifestDigest != subjectDigest {
				lastErr = fmt.Errorf("payload %s signs digest %s, not subject %s", layer.Digest, simple.Critical.Image.DockerManifestDigest, subjectDigest)
				continue // signed a different digest than ref currently resolves to
			}
			return subjectDigest, nil
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("%w for %s (last candidate: %v)", ErrNoValidSignature, ref, lastErr)
	}
	return "", fmt.Errorf("%w for %s (no cosign signature referrer found)", ErrNoValidSignature, ref)
}
