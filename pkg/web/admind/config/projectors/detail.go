package projectors

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// getObj Gets the object at key into obj. It returns (true, nil) when the
// object exists, (false, nil) when it is NotFound (the projector reports a 404
// as a nil detail), and (false, err) for any other read failure. It lets a
// dispatching detail projector (e.g. tools) probe several CRDs at one id.
func getObj(ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) (bool, error) {
	err := c.Get(ctx, key, obj)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// fld is a plain label/value field.
func fld(label, value string) config.Field { return config.Field{Label: label, Value: value} }

// fldLink is a label/value field whose value links to another admin entity's
// detail page. The link is attached only when both value and id are non-empty,
// so an unset ref renders as a bare (linkless) field rather than a dead link.
func fldLink(label, value, entity, id string) config.Field {
	f := config.Field{Label: label, Value: value}
	if value != "" && id != "" {
		f.Link = &config.Link{Entity: entity, ID: id}
	}
	return f
}

// fldHref is a label/value field whose VALUE text links out to an explicit
// external href (scheme-guarded by the frontend). value is the short human
// label the UI shows (e.g. a short commit SHA); href the full link target. A
// field whose value is empty is dropped by fieldsSection, so callers pass an
// empty value+href to omit the field entirely.
func fldHref(label, value, href string) config.Field {
	return config.Field{Label: label, Value: value, Href: href}
}

// fieldsSection builds a "fields" Section, dropping any field with an empty
// value so the tab shows only what is actually set. Returns a zero Section
// (caller should skip it) when nothing survives.
func fieldsSection(id, label string, fields ...config.Field) config.Section {
	kept := make([]config.Field, 0, len(fields))
	for _, f := range fields {
		if f.Value != "" {
			kept = append(kept, f)
		}
	}
	return config.Section{ID: id, Label: label, Kind: config.SectionFields, Fields: kept}
}

// textSection builds a "text" Section.
func textSection(id, label, text string) config.Section {
	return config.Section{ID: id, Label: label, Kind: config.SectionText, Text: text}
}

// listSection builds a "list" Section.
func listSection(id, label string, items []config.ListItem) config.Section {
	return config.Section{ID: id, Label: label, Kind: config.SectionList, Items: items}
}

// channelIdentitiesSection lists the channel-native identities linked to a
// canonical user (one row per (kind, domain, externalID)). Empty → appendSection
// drops it.
func channelIdentitiesSection(ids []spiceboxv1alpha1.ChannelIdentity) config.Section {
	items := make([]config.ListItem, 0, len(ids))
	for _, ci := range ids {
		title := ci.DisplayName
		if title == "" {
			title = ci.ExternalID
		}
		subtitle := ci.Kind + " · " + ci.Domain + " · " + ci.ExternalID
		if ci.Email != "" {
			subtitle += " · " + ci.Email
		}
		items = append(items, config.ListItem{
			Title:    title,
			Subtitle: subtitle,
			Badges:   []config.Badge{{Key: "kind", Value: ci.Kind}},
		})
	}
	return listSection("channelidentities", "Channel identities", items)
}

// appendSection appends s to secs only when it carries content, so an
// all-empty tab (e.g. a Source section for a hand-authored skill) is omitted
// rather than rendered blank.
func appendSection(secs []config.Section, s config.Section) []config.Section {
	if len(s.Fields) == 0 && s.Text == "" && len(s.Items) == 0 {
		return secs
	}
	return append(secs, s)
}

// healthSection returns a "text" Health tab carrying the primary condition's
// full Message when the resource is unhealthy (status != okWord), else a zero
// Section (appendSection then drops it). This surfaces the full degraded
// reason the compact StatusReason truncates to a machine word.
func healthSection(conds []metav1.Condition, condType, okWord, status string) config.Section {
	if status == okWord {
		return config.Section{}
	}
	c := meta.FindStatusCondition(conds, condType)
	msg := ""
	if c != nil {
		msg = c.Message
		if msg == "" {
			msg = c.Reason
		}
	}
	if msg == "" {
		return config.Section{}
	}
	return textSection("health", "Health", msg)
}

// yesNo renders a bool as a compact "yes"/"no" for a field value.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// contains reports whether s is present in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
