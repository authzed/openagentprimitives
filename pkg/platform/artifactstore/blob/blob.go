// Package blob implements artifactstore.Store over gocloud.dev/blob, giving
// one implementation across GCS (gs://), S3 (s3://), Azure Blob (azblob://),
// local disk (file://), and in-process memory (mem://). The scheme comes from
// ARTIFACT_STORE_URL; refs are "<base-url>/<key>" (for mem:// just
// "mem://<key>", byte-identical to the retired pkg/platform/artifactstore/memory).
package blob

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/azureblob" // azblob:// (EKS/AKS automation deferred; URL works today)
	_ "gocloud.dev/blob/fileblob"  // file:// (oap init --local PVC)
	_ "gocloud.dev/blob/gcsblob"   // gs:// via ADC / workload identity
	"gocloud.dev/blob/memblob"     // mem:// (tests, e2e harness)
	_ "gocloud.dev/blob/s3blob"    // s3:// via ambient AWS creds (IRSA)
	"gocloud.dev/gcerrors"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

const (
	defaultLimit = 100
	maxLimit     = 500
)

// Store is the gocloud-backed artifactstore.Store. One Store == one bucket
// (or directory / in-process map); the operator holds exactly one.
type Store struct {
	bucket *blob.Bucket
	base   string // normalized ref base: "gs://bucket", "file:///var/lib/ap/artifacts", "mem://"
}

var _ artifactstore.Store = (*Store)(nil)

// Open opens the store at rawURL (any blank-imported scheme). Query params
// (e.g. file://…?create_dir=true) configure the driver but are stripped from
// the ref base so refs stay clean.
func Open(ctx context.Context, rawURL string) (*Store, error) {
	if !strings.Contains(rawURL, "://") {
		return nil, fmt.Errorf("artifactstore: store URL %q has no scheme (want gs://, s3://, azblob://, file://, or mem://)", rawURL)
	}
	b, err := blob.OpenBucket(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("artifactstore: open bucket %q: %w", rawURL, err)
	}
	base := rawURL
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	if !strings.HasSuffix(base, "://") { // keep "mem://" intact
		base = strings.TrimSuffix(base, "/")
	}
	return &Store{bucket: b, base: base}, nil
}

// NewMem returns a fresh in-process store with refs "mem://<key>" — the
// drop-in replacement for the retired pkg/platform/artifactstore/memory backend in
// tests and the e2e harness.
func NewMem() *Store {
	return &Store{bucket: memblob.OpenBucket(nil), base: "mem://"}
}

// BaseURL returns the normalized ref base (no query params, no trailing "/").
func (s *Store) BaseURL() string { return s.base }

// Close releases the underlying bucket.
func (s *Store) Close() error { return s.bucket.Close() }

func (s *Store) refFor(key string) artifactstore.Ref {
	if strings.HasSuffix(s.base, "://") {
		return artifactstore.Ref(s.base + key)
	}
	return artifactstore.Ref(s.base + "/" + key)
}

// Key is the inverse of Put's ref construction: it returns the storage key a
// Ref of THIS store resolves to, or an error naming both bases for a foreign
// ref (different scheme, bucket, or path).
func (s *Store) Key(ref artifactstore.Ref) (string, error) {
	prefix := s.base
	if !strings.HasSuffix(prefix, "://") {
		prefix += "/"
	}
	r := string(ref)
	if !strings.HasPrefix(r, prefix) || len(r) == len(prefix) {
		return "", fmt.Errorf("artifactstore: foreign ref %q: not under store base %q", ref, s.base)
	}
	return r[len(prefix):], nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader) (artifactstore.Ref, error) {
	w, err := s.bucket.NewWriter(ctx, key, nil)
	if err != nil {
		return "", fmt.Errorf("artifactstore: open writer for %q: %w", key, err)
	}
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close() // best-effort abort; the copy error is the real failure
		return "", fmt.Errorf("artifactstore: write %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("artifactstore: commit %q: %w", key, err)
	}
	return s.refFor(key), nil
}

func (s *Store) Get(ctx context.Context, ref artifactstore.Ref) (io.ReadCloser, error) {
	key, err := s.Key(ref)
	if err != nil {
		// ErrNotFound-class so HTTP paths 404 instead of 500, but the message
		// says WHY (foreign base) so logs are loud, not a silent miss.
		return nil, fmt.Errorf("%w: %v", artifactstore.ErrNotFound, err)
	}
	rc, err := s.bucket.NewReader(ctx, key, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, artifactstore.ErrNotFound
		}
		return nil, fmt.Errorf("artifactstore: read %q: %w", key, err)
	}
	return rc, nil
}

func (s *Store) Delete(ctx context.Context, ref artifactstore.Ref) error {
	key, err := s.Key(ref)
	if err != nil {
		// Idempotent contract: a foreign ref is definitionally not in this
		// store, i.e. already gone. Erroring here would wedge artifactrender
		// finalizers on pre-migration (mem://) refs forever.
		return nil
	}
	if err := s.bucket.Delete(ctx, key); err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil
		}
		return fmt.Errorf("artifactstore: delete %q: %w", key, err)
	}
	return nil
}

func (s *Store) Exists(ctx context.Context, ref artifactstore.Ref) (bool, error) {
	key, err := s.Key(ref)
	if err != nil {
		// Foreign ref: not in this store, so definitionally absent.
		return false, nil
	}
	ok, err := s.bucket.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("artifactstore: exists %q: %w", key, err)
	}
	return ok, nil
}

func (s *Store) List(ctx context.Context, opts artifactstore.ListOpts) ([]artifactstore.Item, string, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	token := blob.FirstPageToken
	if opts.Continue != "" {
		t, err := base64.RawURLEncoding.DecodeString(opts.Continue)
		if err != nil {
			return nil, "", fmt.Errorf("artifactstore: invalid continue token: %w", err)
		}
		token = t
	}
	objs, next, err := s.bucket.ListPage(ctx, token, limit, &blob.ListOptions{Prefix: opts.Prefix})
	if err != nil {
		return nil, "", fmt.Errorf("artifactstore: list (prefix %q): %w", opts.Prefix, err)
	}
	items := make([]artifactstore.Item, 0, len(objs))
	for _, o := range objs {
		if o.IsDir {
			continue
		}
		items = append(items, artifactstore.Item{
			Ref:       s.refFor(o.Key),
			Key:       o.Key,
			Size:      o.Size,
			CreatedAt: o.ModTime,
		})
	}
	nextToken := ""
	if len(next) > 0 {
		nextToken = base64.RawURLEncoding.EncodeToString(next)
	}
	return items, nextToken, nil
}
