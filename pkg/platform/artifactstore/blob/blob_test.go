package blob_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

// openers returns one factory per scheme under contract test. file:// uses a
// per-test tempdir; mem:// is a fresh in-process bucket.
func openers(t *testing.T) map[string]func(t *testing.T) *blobstore.Store {
	t.Helper()
	return map[string]func(t *testing.T) *blobstore.Store{
		"mem": func(t *testing.T) *blobstore.Store {
			s := blobstore.NewMem()
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
		"file": func(t *testing.T) *blobstore.Store {
			s, err := blobstore.Open(context.Background(), "file://"+t.TempDir()+"?create_dir=true")
			require.NoError(t, err, "open file:// store")
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
}

func mustPut(t *testing.T, s *blobstore.Store, key, content string) artifactstore.Ref {
	t.Helper()
	ref, err := s.Put(context.Background(), key, strings.NewReader(content))
	require.NoError(t, err, "Put %q", key)
	return ref
}

func TestPutGetRoundtrip(t *testing.T) {
	for scheme, open := range openers(t) {
		t.Run(scheme, func(t *testing.T) {
			s := open(t)
			ref := mustPut(t, s, "ns/sess/call/stdout", "hello world")
			rc, err := s.Get(context.Background(), ref)
			require.NoError(t, err)
			defer rc.Close()
			b, err := io.ReadAll(rc)
			require.NoError(t, err)
			assert.Equal(t, "hello world", string(b))
		})
	}
}

func TestRefFormatAndKeyInverse(t *testing.T) {
	// mem:// refs must stay byte-identical to the old memory backend
	// ("mem://" + key) — pkg/x/debug parses them; gs:// refs carry the bucket.
	s := blobstore.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	ref := mustPut(t, s, "ns/sess/x", "v")
	assert.Equal(t, artifactstore.Ref("mem://ns/sess/x"), ref)

	key, err := s.Key(ref)
	require.NoError(t, err)
	assert.Equal(t, "ns/sess/x", key)

	for scheme, open := range openers(t) {
		t.Run(scheme+" Key is inverse of Put", func(t *testing.T) {
			st := open(t)
			r := mustPut(t, st, "a/b/c", "v")
			k, err := st.Key(r)
			require.NoError(t, err)
			assert.Equal(t, "a/b/c", k)
		})
	}
}

func TestForeignRefSemantics(t *testing.T) {
	// A ref from a different store base: Get → ErrNotFound-class error whose
	// message names the foreign base (loud in logs, 404 over HTTP); Exists →
	// (false, nil); Delete → nil (idempotent: definitionally gone from THIS
	// store — old mem:// refs must not wedge artifactrender finalizers after
	// a cluster migrates to gs://).
	for scheme, open := range openers(t) {
		t.Run(scheme, func(t *testing.T) {
			s := open(t)
			foreign := artifactstore.Ref("gs://someone-elses-bucket/ns/sess/x")

			_, err := s.Get(context.Background(), foreign)
			require.Error(t, err)
			assert.True(t, errors.Is(err, artifactstore.ErrNotFound), "Get foreign ref is ErrNotFound-class")
			assert.Contains(t, err.Error(), "foreign", "message says WHY")

			ok, err := s.Exists(context.Background(), foreign)
			require.NoError(t, err)
			assert.False(t, ok)

			assert.NoError(t, s.Delete(context.Background(), foreign))

			_, err = s.Key(foreign)
			assert.Error(t, err)
		})
	}
}

func TestGetNotFound(t *testing.T) {
	for scheme, open := range openers(t) {
		t.Run(scheme, func(t *testing.T) {
			s := open(t)
			mustPut(t, s, "exists", "v") // ensure base is live
			_, err := s.Get(context.Background(), artifactstore.Ref(s.BaseURL()+"/nope"))
			assert.True(t, errors.Is(err, artifactstore.ErrNotFound))
		})
	}
}

func TestDeleteIdempotent(t *testing.T) {
	for scheme, open := range openers(t) {
		t.Run(scheme, func(t *testing.T) {
			s := open(t)
			ref := mustPut(t, s, "k", "v")
			require.NoError(t, s.Delete(context.Background(), ref))
			assert.NoError(t, s.Delete(context.Background(), ref), "second delete is a no-op")
			ok, err := s.Exists(context.Background(), ref)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}

func TestKeyCollisionOverwrites(t *testing.T) {
	for scheme, open := range openers(t) {
		t.Run(scheme, func(t *testing.T) {
			s := open(t)
			ref1 := mustPut(t, s, "same", "first")
			ref2 := mustPut(t, s, "same", "second")
			assert.Equal(t, ref1, ref2)
			rc, err := s.Get(context.Background(), ref2)
			require.NoError(t, err)
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			assert.Equal(t, "second", string(b))
		})
	}
}

func TestListPaginatesAndFilters(t *testing.T) {
	for scheme, open := range openers(t) {
		t.Run(scheme, func(t *testing.T) {
			ctx := context.Background()

			t.Run("empty store returns no items and no token", func(t *testing.T) {
				s := open(t)
				items, next, err := s.List(ctx, artifactstore.ListOpts{})
				require.NoError(t, err)
				assert.Empty(t, items)
				assert.Empty(t, next)
			})

			t.Run("full list is key-ascending with sizes and CreatedAt", func(t *testing.T) {
				s := open(t)
				mustPut(t, s, "b", "22")
				mustPut(t, s, "a", "1")
				mustPut(t, s, "c", "333")
				items, next, err := s.List(ctx, artifactstore.ListOpts{})
				require.NoError(t, err)
				require.Len(t, items, 3)
				assert.Empty(t, next)
				assert.Equal(t, []string{"a", "b", "c"}, []string{items[0].Key, items[1].Key, items[2].Key})
				assert.Equal(t, int64(1), items[0].Size)
				assert.False(t, items[0].CreatedAt.IsZero())
				// Ref = base + key, except mem:// which has no path segment
				// separator to add (base already ends "://"; refs stay
				// "mem://<key>", byte-identical to the retired memory backend).
				sep := "/"
				if strings.HasSuffix(s.BaseURL(), "://") {
					sep = ""
				}
				assert.Equal(t, s.BaseURL()+sep+"a", string(items[0].Ref))
			})

			t.Run("prefix filter returns only matching keys", func(t *testing.T) {
				s := open(t)
				mustPut(t, s, "tenant-a/1", "x")
				mustPut(t, s, "tenant-a/2", "x")
				mustPut(t, s, "tenant-b/1", "x")
				items, next, err := s.List(ctx, artifactstore.ListOpts{Prefix: "tenant-a/"})
				require.NoError(t, err)
				assert.Empty(t, next)
				require.Len(t, items, 2)
				for _, it := range items {
					assert.True(t, strings.HasPrefix(it.Key, "tenant-a/"))
				}
			})

			t.Run("pagination via continue token traverses all pages", func(t *testing.T) {
				s := open(t)
				for _, k := range []string{"e", "b", "d", "a", "c"} {
					mustPut(t, s, k, "v")
				}
				var got []string
				token := ""
				pages := 0
				for {
					items, next, err := s.List(ctx, artifactstore.ListOpts{Limit: 2, Continue: token})
					require.NoError(t, err)
					pages++
					require.LessOrEqual(t, pages, 5, "runaway pagination")
					for _, it := range items {
						got = append(got, it.Key)
					}
					if next == "" {
						break
					}
					token = next
				}
				assert.Equal(t, []string{"a", "b", "c", "d", "e"}, got)
			})

			t.Run("bad continue token errors", func(t *testing.T) {
				s := open(t)
				_, _, err := s.List(ctx, artifactstore.ListOpts{Continue: "!!not-base64!!"})
				assert.Error(t, err)
			})
		})
	}
}

func TestOpenNormalizesBase(t *testing.T) {
	dir := t.TempDir()
	// Trailing slash AND a driver query param — both must be stripped from
	// the ref base so refs stay clean; a regression here would make BaseURL()
	// and refFor() equally wrong and thus invisible to BaseURL()-derived
	// assertions, so this pins the exact expected literal.
	s, err := blobstore.Open(context.Background(), "file://"+dir+"/?create_dir=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	assert.Equal(t, "file://"+dir, s.BaseURL(), "query param and trailing slash stripped from base")

	ref := mustPut(t, s, "k", "v")
	assert.Equal(t, artifactstore.Ref("file://"+dir+"/k"), ref, "ref has clean base, no query fragment")
}

func TestOpenRejectsBadURLs(t *testing.T) {
	cases := []struct{ name, url string }{
		{name: "no scheme", url: "/just/a/path"},
		{name: "unregistered scheme", url: "ftp://host/x"},
		{name: "empty", url: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := blobstore.Open(context.Background(), tc.url)
			assert.Error(t, err)
		})
	}
}
