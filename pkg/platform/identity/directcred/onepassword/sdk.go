package onepassword

import (
	"context"
	"fmt"

	opsdk "github.com/1password/onepassword-sdk-go"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// integrationName is the name reported to 1Password via WithIntegrationInfo.
// Paired with apimage.Version so a vault's access log identifies this project.
const integrationName = "agentprimitives"

// init asserts, at process start, that source.go's plain-string bridge
// constants (fieldTypeTOTP, autofillAnywhere, autofillExactDomain) still
// match the SDK's own enum values they mirror. source.go deliberately
// duplicates these as plain strings so that file needs no SDK import (see its
// "layout follows a strict seam" doc comment) -- but that duplication is
// exactly the kind of coupling that rots silently: a future SDK rename or
// re-valued constant would otherwise desync source.go's classification (TOTP
// field detection, autofill-behavior matching) with NO error and NO panic --
// items would just silently stop being treated as fillable or OTP-bearing.
// This panics instead, turning that into an immediate, loud failure on
// process start (and so on every test run and every build-time smoke check),
// not a feature that quietly degrades. sdk.go is the only file importing the
// SDK (package doc comment on source.go), so this is also the only file that
// CAN make this comparison.
//
// Paired with source.go's own comment at the fieldTypeTOTP/autofillAnywhere/
// autofillExactDomain declarations, and with the analogous textual-coupling
// pin in pkg/platform/identity/setup/builtins/onepassword_service_account/flow.go
// (that one pins Verify's error-message substrings instead of an SDK enum).
func init() {
	assertSDKBridge("fieldTypeTOTP", fieldTypeTOTP, string(opsdk.ItemFieldTypeTOTP))
	assertSDKBridge("autofillAnywhere", autofillAnywhere, string(opsdk.AutofillBehaviorAnywhereOnWebsite))
	assertSDKBridge("autofillExactDomain", autofillExactDomain, string(opsdk.AutofillBehaviorExactDomain))
}

// assertSDKBridge panics when a source.go plain-string bridge constant has
// drifted from the SDK enum value it mirrors -- see the init() comment above.
func assertSDKBridge(name, local, sdk string) {
	if local != sdk {
		panic(fmt.Sprintf(
			"onepassword: bridge constant %s = %q no longer matches the SDK's %q -- update source.go's plain-string constant to match the 1Password SDK enum rename",
			name, local, sdk))
	}
}

// sdkClient is the real client seam implementation: the one place the
// 1Password SDK is imported. Every method is an unconditionally boring
// projection of an SDK call onto the package-internal neutral types in
// source.go -- no mapping, filtering, or policy decision beyond "which
// category counts as a login," which the List contract names here. All the
// branching that a test needs to cover lives in source.go.
//
// token is retained for ONE reason: to scrub it from any error the SDK core
// returns, so a bad-token failure can never carry the token into a log line
// (see scrubToken in source.go). It is never otherwise read.
type sdkClient struct {
	oc    *opsdk.Client
	token string
}

var _ client = (*sdkClient)(nil)

// newSDKClient builds a service-account-authenticated SDK client. It matches
// the Source.newClient signature so New can wire it as the default. On error
// it returns a genuine nil interface (never a typed-nil *sdkClient).
func newSDKClient(ctx context.Context, token string) (client, error) {
	oc, err := opsdk.NewClient(ctx,
		opsdk.WithServiceAccountToken(token),
		opsdk.WithIntegrationInfo(integrationName, apimage.Version),
	)
	if err != nil {
		return nil, scrubToken(err, token)
	}
	return &sdkClient{oc: oc, token: token}, nil
}

func (c *sdkClient) ListVaults(ctx context.Context) ([]vault, error) {
	overviews, err := c.oc.Vaults().List(ctx)
	if err != nil {
		return nil, scrubToken(err, c.token)
	}
	out := make([]vault, 0, len(overviews))
	for _, v := range overviews {
		out = append(out, vault{ID: v.ID, Title: v.Title})
	}
	return out, nil
}

func (c *sdkClient) ListLoginItems(ctx context.Context, vaultID string) ([]listItem, error) {
	overviews, err := c.oc.Items().List(ctx, vaultID,
		opsdk.NewItemListFilterTypeVariantByState(&opsdk.ItemListFilterByStateInner{Active: true}),
	)
	if err != nil {
		return nil, scrubToken(err, c.token)
	}
	out := make([]listItem, 0, len(overviews))
	for _, o := range overviews {
		if o.Category != opsdk.ItemCategoryLogin {
			continue
		}
		out = append(out, listItem{
			ID:       o.ID,
			Title:    o.Title,
			Websites: projectWebsites(o.Websites),
		})
	}
	return out, nil
}

func (c *sdkClient) GetItem(ctx context.Context, vaultID, itemID string) (itemDetail, error) {
	item, err := c.oc.Items().Get(ctx, vaultID, itemID)
	if err != nil {
		return itemDetail{}, scrubToken(err, c.token)
	}
	fields := make([]field, 0, len(item.Fields))
	for _, f := range item.Fields {
		fields = append(fields, field{
			ID:        f.ID,
			FieldType: string(f.FieldType),
			Value:     f.Value,
			OTP:       projectOTP(f),
		})
	}
	return itemDetail{ID: item.ID, Title: item.Title, Fields: fields}, nil
}

// projectWebsites copies the SDK's autofill-behavior string through verbatim;
// the AnywhereOnWebsite/ExactDomain/Never decision is source.go's.
func projectWebsites(sites []opsdk.Website) []website {
	out := make([]website, 0, len(sites))
	for _, w := range sites {
		out = append(out, website{URL: w.URL, Autofill: string(w.AutofillBehavior)})
	}
	return out
}

// projectOTP surfaces a TOTP field's freshly computed code (and any compute
// error) from the SDK's Details envelope. Nil for non-TOTP fields.
func projectOTP(f opsdk.ItemField) *otp {
	if f.FieldType != opsdk.ItemFieldTypeTOTP || f.Details == nil {
		return nil
	}
	d := f.Details.OTP()
	if d == nil {
		return &otp{}
	}
	o := &otp{}
	if d.Code != nil {
		o.Code = *d.Code
	}
	if d.ErrorMessage != nil {
		o.Error = *d.ErrorMessage
	}
	return o
}
