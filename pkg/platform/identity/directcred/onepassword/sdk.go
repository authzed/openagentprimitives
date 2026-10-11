package onepassword

import (
	"context"

	opsdk "github.com/1password/onepassword-sdk-go"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// integrationName is the name reported to 1Password via WithIntegrationInfo.
// Paired with apimage.Version so a vault's access log identifies this project.
const integrationName = "agentprimitives"

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
