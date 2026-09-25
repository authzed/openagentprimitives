package v1alpha1_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestValidateEnvDefaults asserts the reserved-name gate: env keys that
// exactly match an auth-injected env-var name must be rejected so an
// operator-controlled spec cannot shadow a binding-controlled credential
// before the runner attaches it. Match is exact, not prefix — GITHUB_HOST
// is not a credential and must pass even though GITHUB_TOKEN is reserved.
func TestValidateEnvDefaults(t *testing.T) {
	// The reserved set the spiceboxclass reconciler derives today from
	// the builtin toolkits (gh → GITHUB_TOKEN, claude → ANTHROPIC_API_KEY).
	reserved := []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY"}

	cases := []struct {
		name         string
		env          map[string]string
		reserved     []string
		wantErr      bool
		wantContains []string
		wantErrExact string
	}{
		{name: "nil env: clean", env: nil, reserved: reserved, wantErr: false},
		{name: "nil reserved: clean", env: map[string]string{"GITHUB_TOKEN": "x"}, reserved: nil, wantErr: false},
		{name: "empty reserved: clean", env: map[string]string{"GITHUB_TOKEN": "x"}, reserved: []string{}, wantErr: false},
		{name: "GIT_DIR: clean", env: map[string]string{"GIT_DIR": "/var/ap-git/.git"}, reserved: reserved, wantErr: false},
		{name: "GIT_WORK_TREE: clean", env: map[string]string{"GIT_WORK_TREE": "/workspace"}, reserved: reserved, wantErr: false},
		{
			name:         "GITHUB_TOKEN exact match: rejected",
			env:          map[string]string{"GITHUB_TOKEN": "x"},
			reserved:     reserved,
			wantErr:      true,
			wantContains: []string{"GITHUB_TOKEN"},
		},
		{
			name:     "GITHUB_HOST shares prefix but not name: allowed",
			env:      map[string]string{"GITHUB_HOST": "ghe.example.com"},
			reserved: reserved,
			wantErr:  false,
		},
		{
			name:         "ANTHROPIC_API_KEY exact match: rejected",
			env:          map[string]string{"ANTHROPIC_API_KEY": "x"},
			reserved:     reserved,
			wantErr:      true,
			wantContains: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name:         "mixed: rejection lists all bad keys",
			env:          map[string]string{"GIT_DIR": "/p", "GITHUB_TOKEN": "x", "ANTHROPIC_API_KEY": "y"},
			reserved:     reserved,
			wantErr:      true,
			wantContains: []string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY"},
		},
		{
			// Regression case: PATH/TMPDIR are toolchain-computed, not
			// toolkit-credential-reserved, so `reserved` is nil/empty here.
			// The toolchain-key check must run BEFORE the `len(reserved) == 0
			// || len(env) == 0` early return or this case wrongly passes.
			name:         "PATH set with no toolkit-reserved names: still rejected (toolchain-computed)",
			env:          map[string]string{"PATH": "/usr/bin"},
			reserved:     nil,
			wantErr:      true,
			wantContains: []string{"PATH", "spec.toolchains"},
		},
		{
			name:         "TMPDIR set with empty reserved slice: still rejected (toolchain-computed)",
			env:          map[string]string{"TMPDIR": "/var/ap-cache/tmp"},
			reserved:     []string{},
			wantErr:      true,
			wantContains: []string{"TMPDIR", "spec.toolchains"},
		},
		{
			// Regression case for the non-determinism bug: ranging the map and
			// returning on the first hit would make this message vary randomly
			// between runs. Assert the exact, sorted message so any regression to
			// the map-range-and-return-first-hit shape fails this test.
			name:         "PATH and TMPDIR both set: rejection message is deterministic and lists both, sorted",
			env:          map[string]string{"TMPDIR": "/var/ap-cache/tmp", "PATH": "/usr/bin"},
			reserved:     nil,
			wantErr:      true,
			wantErrExact: `envDefaults contains reserved key(s) computed from spec.toolchains: [PATH TMPDIR]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v1alpha1.ValidateEnvDefaults(tc.env, tc.reserved)
			if tc.wantErr {
				assert.Error(t, err)
				for _, want := range tc.wantContains {
					assert.Contains(t, err.Error(), want, "error should mention %q", want)
				}
				if tc.wantErrExact != "" {
					assert.Equal(t, tc.wantErrExact, err.Error(), "error message must be deterministic across reconciles")
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestValidatePrivateVolumes asserts the collision gate: a PrivateVolume
// whose name or mountPath would clash with a pod-builder-reserved volume,
// a ConfigMap mount, or another PrivateVolume must be rejected at class
// validation — before the malformed spec reaches pod-apply, where the
// only signal is a Pending pod with a duplicate-volume API error.
func TestValidatePrivateVolumes(t *testing.T) {
	cfgMount := []v1alpha1.SpiceboxMount{{Name: "cfg", MountPath: "/etc/cfg"}}

	cases := []struct {
		name         string
		pvs          []v1alpha1.PrivateVolume
		mounts       []v1alpha1.SpiceboxMount
		wantErr      bool
		wantContains []string
	}{
		{name: "nil: clean", pvs: nil, wantErr: false},
		{name: "empty slice: clean", pvs: []v1alpha1.PrivateVolume{}, wantErr: false},
		{
			name:    "one valid: clean",
			pvs:     []v1alpha1.PrivateVolume{{Name: "ap-git", MountPath: "/var/ap-git"}},
			wantErr: false,
		},
		{
			name: "two valid: clean",
			pvs: []v1alpha1.PrivateVolume{
				{Name: "ap-git", MountPath: "/var/ap-git"},
				{Name: "ap-scratch", MountPath: "/var/ap-scratch"},
			},
			wantErr: false,
		},
		{
			name:         "empty name: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "", MountPath: "/var/x"}},
			wantErr:      true,
			wantContains: []string{"name is required"},
		},
		{
			name:         "empty mountPath: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "ap-git", MountPath: ""}},
			wantErr:      true,
			wantContains: []string{"mountPath is required"},
		},
		{
			name:         "relative mountPath: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "ap-git", MountPath: "relative/path"}},
			wantErr:      true,
			wantContains: []string{"must be absolute"},
		},
		{
			name:         "reserved name workspace: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "workspace", MountPath: "/var/x"}},
			wantErr:      true,
			wantContains: []string{"reserved volume name"},
		},
		{
			name:         "reserved name work: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "work", MountPath: "/var/x"}},
			wantErr:      true,
			wantContains: []string{"reserved volume name"},
		},
		{
			name:         "name collides with ConfigMap mount: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "cfg", MountPath: "/var/x"}},
			mounts:       cfgMount,
			wantErr:      true,
			wantContains: []string{"ConfigMap mount name"},
		},
		{
			name:         "reserved mountPath /tmp: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "ap-git", MountPath: "/tmp"}},
			wantErr:      true,
			wantContains: []string{"reserved mount path"},
		},
		{
			name: "duplicate name within slice: rejected",
			pvs: []v1alpha1.PrivateVolume{
				{Name: "ap-git", MountPath: "/var/a"},
				{Name: "ap-git", MountPath: "/var/b"},
			},
			wantErr:      true,
			wantContains: []string{"duplicates"},
		},
		{
			name: "duplicate mountPath within slice: rejected",
			pvs: []v1alpha1.PrivateVolume{
				{Name: "ap-git", MountPath: "/var/x"},
				{Name: "ap-cache", MountPath: "/var/x"},
			},
			wantErr:      true,
			wantContains: []string{"duplicates"},
		},
		{
			name:         "non-DNS-1123 name: rejected",
			pvs:          []v1alpha1.PrivateVolume{{Name: "Bad_Name", MountPath: "/var/x"}},
			wantErr:      true,
			wantContains: []string{"DNS-1123 label"},
		},
		{
			name: "multi-problem: error mentions all problems",
			pvs: []v1alpha1.PrivateVolume{
				{Name: "workspace", MountPath: "/tmp"},
				{Name: "Bad_Name", MountPath: "relative"},
			},
			wantErr: true,
			wantContains: []string{
				"reserved volume name",
				"reserved mount path",
				"DNS-1123 label",
				"must be absolute",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v1alpha1.ValidatePrivateVolumes(tc.pvs, tc.mounts)
			if tc.wantErr {
				assert.Error(t, err)
				for _, want := range tc.wantContains {
					assert.Contains(t, err.Error(), want, "error should mention %q", want)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateScratchSizes(t *testing.T) {
	res := func(mem, tmp, work string) v1alpha1.SpiceboxResources {
		r := v1alpha1.SpiceboxResources{
			CPU:              resource.MustParse("1"),
			Memory:           resource.MustParse(mem),
			EphemeralStorage: resource.MustParse("1Gi"),
		}
		if tmp != "" {
			q := resource.MustParse(tmp)
			r.TmpSize = &q
		}
		if work != "" {
			q := resource.MustParse(work)
			r.WorkSize = &q
		}
		return r
	}

	cases := []struct {
		name    string
		res     v1alpha1.SpiceboxResources
		wantErr string
	}{
		{name: "unset sizes fall back to defaults: accepted", res: res("1Gi", "", "")},
		{name: "explicit sizes: accepted", res: res("4Gi", "512Mi", "512Mi")},
		{
			// sizeLimit is a CAP, not a reservation — tmpfs pages are charged to
			// the memory limit only as written. Many shipped classes run 64Mi of
			// memory with the default 50Mi/100Mi caps; rejecting that shape would
			// be both wrong and breaking.
			name: "caps larger than the memory limit: accepted (a cap is not a reservation)",
			res:  res("64Mi", "", ""),
		},
		{
			name: "caps far larger than the memory limit: still accepted",
			res:  res("1Gi", "2Gi", "100Mi"),
		},
		{
			name:    "zero tmpSize: rejected",
			res:     res("1Gi", "0", "100Mi"),
			wantErr: "tmpSize",
		},
		{
			name:    "zero workSize: rejected",
			res:     res("1Gi", "50Mi", "0"),
			wantErr: "workSize",
		},
		{
			name: "zero cacheSize: rejected",
			res: func() v1alpha1.SpiceboxResources {
				r := res("1Gi", "50Mi", "100Mi")
				q := resource.MustParse("0")
				r.CacheSize = &q
				return r
			}(),
			wantErr: "cacheSize",
		},
		{
			name: "explicit cacheSize: accepted",
			res: func() v1alpha1.SpiceboxResources {
				r := res("1Gi", "50Mi", "100Mi")
				q := resource.MustParse("8Gi")
				r.CacheSize = &q
				return r
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v1alpha1.ValidateScratchSizes(tc.res)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
