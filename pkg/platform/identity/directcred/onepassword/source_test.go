package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
)

// tokenData is the minimal valid SecretData: a non-empty service-account
// token under the "token" key.
func tokenData() map[string][]byte {
	return map[string][]byte{"token": []byte("fake-sa-token")}
}

// fakeClient implements the package-internal client seam without the SDK's
// wasm core, so the mapping/Verify/List/Fetch logic runs under test.
type fakeClient struct {
	vaults     []vault
	loginItems map[string][]listItem // keyed by vaultID
	items      map[string]itemDetail // keyed by itemID

	listVaultsErr error
	listItemsErr  error
	getItemErr    error

	getItemCalls int
	// otpCodes rotates a fresh code per GetItem call, so a test can prove each
	// OTP() invocation re-reads the item rather than caching.
	otpCodes []string
}

func (f *fakeClient) ListVaults(context.Context) ([]vault, error) {
	if f.listVaultsErr != nil {
		return nil, f.listVaultsErr
	}
	return f.vaults, nil
}

func (f *fakeClient) ListLoginItems(_ context.Context, vaultID string) ([]listItem, error) {
	if f.listItemsErr != nil {
		return nil, f.listItemsErr
	}
	return f.loginItems[vaultID], nil
}

func (f *fakeClient) GetItem(_ context.Context, _ string, itemID string) (itemDetail, error) {
	f.getItemCalls++
	if f.getItemErr != nil {
		return itemDetail{}, f.getItemErr
	}
	it, ok := f.items[itemID]
	if !ok {
		return itemDetail{}, fmt.Errorf("fake: no item %q", itemID)
	}
	out := itemDetail{ID: it.ID, Title: it.Title}
	for _, fld := range it.Fields {
		nf := fld
		if nf.FieldType == fieldTypeTOTP && len(f.otpCodes) > 0 {
			code := f.otpCodes[(f.getItemCalls-1)%len(f.otpCodes)]
			nf.OTP = &otp{Code: code}
		}
		out.Fields = append(out.Fields, nf)
	}
	return out, nil
}

func sourceWith(c client) *Source {
	return &Source{newClient: func(context.Context, string) (client, error) { return c, nil }}
}

func sourceWithClientErr(err error) *Source {
	return &Source{newClient: func(context.Context, string) (client, error) { return nil, err }}
}

func TestKind(t *testing.T) {
	assert.Equal(t, directcred.SourceOnePassword, New().Kind())
}

func TestListWebsiteMapping(t *testing.T) {
	cases := []struct {
		name  string
		items []listItem
		want  []directcred.Ref
	}{
		{
			name:  "AnywhereOnWebsite maps to a suffix (non-exact) host",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "example.com", Autofill: autofillAnywhere}}}},
			want:  []directcred.Ref{{ID: "i1", Label: "A", Hosts: []directcred.Host{{Name: "example.com", Exact: false}}}},
		},
		{
			name:  "ExactDomain maps to an exact host",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "app.example.com", Autofill: autofillExactDomain}}}},
			want:  []directcred.Ref{{ID: "i1", Label: "A", Hosts: []directcred.Host{{Name: "app.example.com", Exact: true}}}},
		},
		{
			name:  "Never is dropped; an item with no other usable host is omitted",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "example.com", Autofill: "Never"}}}},
			want:  []directcred.Ref{},
		},
		{
			name:  "URL-shaped value: hostname is parsed out of a full URL",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "https://sub.example.com/login?next=1", Autofill: autofillAnywhere}}}},
			want:  []directcred.Ref{{ID: "i1", Label: "A", Hosts: []directcred.Host{{Name: "sub.example.com", Exact: false}}}},
		},
		{
			name:  "URL-shaped value with a port: port stripped, host kept",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "example.com:8443", Autofill: autofillExactDomain}}}},
			want:  []directcred.Ref{{ID: "i1", Label: "A", Hosts: []directcred.Host{{Name: "example.com", Exact: true}}}},
		},
		{
			name:  "uppercase host is canonicalized to lowercase",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "EXAMPLE.COM", Autofill: autofillAnywhere}}}},
			want:  []directcred.Ref{{ID: "i1", Label: "A", Hosts: []directcred.Host{{Name: "example.com", Exact: false}}}},
		},
		{
			name:  "a host the policy rejects is dropped; item with only that host omitted",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "bad host with spaces", Autofill: autofillAnywhere}}}},
			want:  []directcred.Ref{},
		},
		{
			name: "mixed within one item: Never dropped, Anywhere and Exact kept in order",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{
				{URL: "a.com", Autofill: autofillAnywhere},
				{URL: "b.com", Autofill: "Never"},
				{URL: "c.com", Autofill: autofillExactDomain},
			}}},
			want: []directcred.Ref{{ID: "i1", Label: "A", Hosts: []directcred.Host{
				{Name: "a.com", Exact: false},
				{Name: "c.com", Exact: true},
			}}},
		},
		{
			name: "multiple items sorted by ID; a no-usable-host item is omitted",
			items: []listItem{
				{ID: "i2", Title: "Two", Websites: []website{{URL: "two.com", Autofill: autofillAnywhere}}},
				{ID: "i1", Title: "One", Websites: []website{{URL: "one.com", Autofill: autofillAnywhere}}},
				{ID: "i3", Title: "Three", Websites: []website{{URL: "three.com", Autofill: "Never"}}},
			},
			want: []directcred.Ref{
				{ID: "i1", Label: "One", Hosts: []directcred.Host{{Name: "one.com", Exact: false}}},
				{ID: "i2", Label: "Two", Hosts: []directcred.Host{{Name: "two.com", Exact: false}}},
			},
		},
		{
			name:  "an unknown autofill behavior is dropped (fail-closed)",
			items: []listItem{{ID: "i1", Title: "A", Websites: []website{{URL: "example.com", Autofill: "SomethingNew"}}}},
			want:  []directcred.Ref{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeClient{
				vaults:     []vault{{ID: "v1", Title: "Vault"}},
				loginItems: map[string][]listItem{"v1": tc.items},
			}
			got, err := sourceWith(fake).List(context.Background(),
				directcred.Config{SecretData: tokenData(), VaultID: "v1"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestListRefsCarryNoFieldValues(t *testing.T) {
	const sentinel = "S3NT1NEL-PASSWORD-MUST-NOT-LEAK"
	fake := &fakeClient{
		vaults: []vault{{ID: "v1", Title: "Vault"}},
		loginItems: map[string][]listItem{"v1": {
			{ID: "i1", Title: "Example Login", Websites: []website{{URL: "example.com", Autofill: autofillAnywhere}}},
		}},
		// The sentinel lives ONLY in the item's field detail (GetItem). If List
		// ever started embedding field values into a Ref, this would catch it.
		items: map[string]itemDetail{"i1": {ID: "i1", Title: "Example Login", Fields: []field{
			{ID: "password", FieldType: "Concealed", Value: sentinel},
		}}},
	}

	refs, err := sourceWith(fake).List(context.Background(),
		directcred.Config{SecretData: tokenData(), VaultID: "v1"})
	require.NoError(t, err)

	b, err := json.Marshal(refs)
	require.NoError(t, err)
	assert.NotContains(t, string(b), sentinel)
}

func TestVerify(t *testing.T) {
	cases := []struct {
		name     string
		vaults   []vault
		cfgVault string
		wantPin  directcred.Pin
		wantErr  []string // all substrings must be present; empty means no error
	}{
		{
			name:     "one vault matching the configured name: Pin",
			vaults:   []vault{{ID: "v1", Title: "Prod"}},
			cfgVault: "Prod",
			wantPin:  directcred.Pin{VaultID: "v1", VaultName: "Prod"},
		},
		{
			name:    "one vault, no name configured: pins the single vault",
			vaults:  []vault{{ID: "v1", Title: "OnlyOne"}},
			wantPin: directcred.Pin{VaultID: "v1", VaultName: "OnlyOne"},
		},
		{
			name:     "one vault with the wrong name: error naming both",
			vaults:   []vault{{ID: "v1", Title: "Staging"}},
			cfgVault: "Prod",
			wantErr:  []string{"Staging", "Prod"},
		},
		{
			name:     "two vaults: error listing their names and the count",
			vaults:   []vault{{ID: "v1", Title: "Prod"}, {ID: "v2", Title: "Staging"}},
			cfgVault: "Prod",
			wantErr:  []string{"found 2", "Prod", "Staging"},
		},
		{
			name:     "zero vaults: error naming the count",
			vaults:   nil,
			cfgVault: "Prod",
			wantErr:  []string{"found 0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeClient{vaults: tc.vaults}
			pin, err := sourceWith(fake).Verify(context.Background(),
				directcred.Config{SecretData: tokenData(), Vault: tc.cfgVault})

			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, want := range tc.wantErr {
					assert.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPin, pin)
		})
	}
}

func TestVerifyListVaultsError(t *testing.T) {
	fake := &fakeClient{listVaultsErr: errors.New("boom")}
	_, err := sourceWith(fake).Verify(context.Background(),
		directcred.Config{SecretData: tokenData(), Vault: "Prod"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing vaults")
	assert.Contains(t, err.Error(), "boom")
}

func TestVerifyAuthErrorNeverLeaksToken(t *testing.T) {
	const token = "SUPER-SECRET-SA-TOKEN"
	// The client constructor fails with an error that embeds the token, the
	// way a careless SDK core error could. Verify must scrub it.
	s := sourceWithClientErr(fmt.Errorf("auth failed for token %s", token))
	_, err := s.Verify(context.Background(),
		directcred.Config{SecretData: map[string][]byte{"token": []byte(token)}, Vault: "Prod"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), token, "the service-account token must never appear in an error")
}

// TestVerifyErrorTextMatchesSetupFlowCoupling pins the two exact substrings
// pkg/platform/identity/setup/builtins/onepassword_service_account/flow.go's
// classifyVerifyErr depends on to tell apart WHY Verify failed, since
// Verify returns a single untyped error for every failure mode:
//
//   - vaultCountRe matches "accessible vault, found <N>" to classify a
//     zero/multi-vault scoping failure as VerifyForbidden.
//   - sdkClientErrPrefix matches the literal prefix "onepassword: creating
//     client:" to classify an SDK client-construction failure as
//     VerifyRejected.
//
// These are substring/prefix assertions, not full-message equality, so the
// pin tracks only the two fragments the setup flow actually parses. If
// either wording changes here without updating flow.go's constants, THIS
// test fails loudly on the emission side instead of the setup flow silently
// degrading every such failure to VerifyIndeterminate.
func TestVerifyErrorTextMatchesSetupFlowCoupling(t *testing.T) {
	t.Run("zero/multi-vault scoping failure contains the vaultCountRe substring", func(t *testing.T) {
		fake := &fakeClient{vaults: []vault{{ID: "v1", Title: "Prod"}, {ID: "v2", Title: "Staging"}}}
		_, err := sourceWith(fake).Verify(context.Background(),
			directcred.Config{SecretData: tokenData(), Vault: "Prod"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "accessible vault, found 2",
			"flow.go's vaultCountRe (`accessible vault, found (\\d+)`) depends on this exact wording")
	})

	t.Run("SDK client-construction failure has the sdkClientErrPrefix prefix", func(t *testing.T) {
		s := sourceWithClientErr(errors.New("invalid token format"))
		_, err := s.Verify(context.Background(),
			directcred.Config{SecretData: tokenData(), Vault: "Prod"})
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "onepassword: creating client:"),
			"flow.go's sdkClientErrPrefix does strings.HasPrefix against exactly this text; got %q", err.Error())
	})
}

func TestVerifyMissingToken(t *testing.T) {
	// newClient must never even be called when the token key is absent.
	called := false
	s := &Source{newClient: func(context.Context, string) (client, error) {
		called = true
		return &fakeClient{}, nil
	}}
	_, err := s.Verify(context.Background(), directcred.Config{SecretData: map[string][]byte{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
	assert.False(t, called, "newClient must not be called without a token")
}

func TestFetchFieldSelection(t *testing.T) {
	fake := &fakeClient{
		vaults: []vault{{ID: "v1", Title: "Vault"}},
		items: map[string]itemDetail{"i1": {ID: "i1", Title: "Login", Fields: []field{
			{ID: "username", FieldType: "Text", Value: "alice"},
			{ID: "password", FieldType: "Concealed", Value: "s3cret"},
			{ID: "notesPlain", FieldType: "Text", Value: "ignore me"},
		}}},
	}
	cred, err := sourceWith(fake).Fetch(context.Background(),
		directcred.Config{SecretData: tokenData(), VaultID: "v1"}, "i1")
	require.NoError(t, err)
	assert.Equal(t, "alice", cred.Username)
	assert.Equal(t, "s3cret", cred.Password)
	assert.Nil(t, cred.OTP, "no TOTP field present: OTP must be nil")
}

func TestFetchOTPFreshPerCall(t *testing.T) {
	fake := &fakeClient{
		vaults: []vault{{ID: "v1", Title: "Vault"}},
		items: map[string]itemDetail{"i1": {ID: "i1", Title: "Login", Fields: []field{
			{ID: "username", FieldType: "Text", Value: "alice"},
			{ID: "password", FieldType: "Concealed", Value: "s3cret"},
			{ID: "onetimepassword", FieldType: fieldTypeTOTP, Value: "otpauth://totp/x?secret=abc"},
		}}},
		otpCodes: []string{"111111", "222222"},
	}
	cred, err := sourceWith(fake).Fetch(context.Background(),
		directcred.Config{SecretData: tokenData(), VaultID: "v1"}, "i1")
	require.NoError(t, err)
	require.NotNil(t, cred.OTP, "a TOTP field is present: OTP must be non-nil")

	callsAfterFetch := fake.getItemCalls
	code1, err := cred.OTP()
	require.NoError(t, err)
	code2, err := cred.OTP()
	require.NoError(t, err)

	// Each OTP() must re-read the item (two more GetItem calls) rather than
	// cache, and surface the freshly computed code each time.
	assert.Equal(t, callsAfterFetch+2, fake.getItemCalls, "each OTP() call must re-read the item")
	assert.NotEqual(t, code1, code2, "a re-read must surface a fresh code")
}

func TestFetchOTPComputeError(t *testing.T) {
	fake := &fakeClient{
		vaults: []vault{{ID: "v1", Title: "Vault"}},
		items: map[string]itemDetail{"i1": {ID: "i1", Title: "Login", Fields: []field{
			{ID: "onetimepassword", FieldType: fieldTypeTOTP, Value: "bad", OTP: &otp{Error: "invalid seed"}},
		}}},
		// No otpCodes, so GetItem leaves the seeded OTP (with its Error) intact.
	}
	cred, err := sourceWith(fake).Fetch(context.Background(),
		directcred.Config{SecretData: tokenData(), VaultID: "v1"}, "i1")
	require.NoError(t, err)
	require.NotNil(t, cred.OTP)

	_, err = cred.OTP()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid seed")
}

func TestFetchUnknownItem(t *testing.T) {
	fake := &fakeClient{
		vaults: []vault{{ID: "v1", Title: "Vault"}},
		items:  map[string]itemDetail{},
	}
	_, err := sourceWith(fake).Fetch(context.Background(),
		directcred.Config{SecretData: tokenData(), VaultID: "v1"}, "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

func TestListResolvesVaultByName(t *testing.T) {
	fake := &fakeClient{
		vaults: []vault{{ID: "v1", Title: "Prod"}, {ID: "v2", Title: "Staging"}},
		loginItems: map[string][]listItem{"v2": {
			{ID: "i1", Title: "A", Websites: []website{{URL: "example.com", Autofill: autofillAnywhere}}},
		}},
	}
	// No VaultID set: List must resolve "Staging" to v2 via ListVaults.
	refs, err := sourceWith(fake).List(context.Background(),
		directcred.Config{SecretData: tokenData(), Vault: "Staging"})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "i1", refs[0].ID)
}
