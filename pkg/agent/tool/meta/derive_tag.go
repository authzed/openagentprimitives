package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// maxDeriveInputs bounds how many sources one derive_tag call may name. A
// synthesis draws on a handful of tagged data; a request naming dozens is more
// likely a confusion than a real derivation, and the bound keeps one call's
// SpiceDB access-checks bounded too.
const maxDeriveInputs = 32

// DeriveTagConfig wires derive_tag.
type DeriveTagConfig struct {
	// Mint authorizes each input id against this session's pt_tag#access,
	// VALIDATES (via the dedicated derivation validator) that content is a
	// faithful transformation of those sources, and — only if valid — mints a
	// DERIVED tag STORING content, returning its id. nil ⇒ the tool refuses every
	// call. Any failure (access denied, validation rejected, mint error) is
	// returned so the tool surfaces it rather than tagging content the platform
	// could not vouch for.
	Mint func(ctx context.Context, derivedFrom []string, content string) (string, error)
}

// NewDeriveTag builds the tool the model calls to tag content it synthesizes
// from several tagged sources.
func NewDeriveTag(cfg DeriveTagConfig) tool.Tool { return &deriveTagTool{cfg: cfg} }

type deriveTagTool struct{ cfg DeriveTagConfig }

func (*deriveTagTool) Name() string    { return "derive_tag" }
func (*deriveTagTool) Kind() tool.Kind { return tool.KindMeta }
func (*deriveTagTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*deriveTagTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*deriveTagTool) Description() string {
	return "Tag content you SYNTHESIZE from tagged sources — a figure computed from two tagged numbers, a conclusion " +
		"that fuses two tagged documents — when no single source's tag correctly describes the result. Pass the ids of " +
		"the pt-untrusted regions your synthesis draws on AND the synthesized content itself. A dedicated validator " +
		"checks the content introduces nothing beyond those sources; if it passes you get back a ready-to-paste " +
		"pt-untrusted region to insert VERBATIM into your reply. You can only ever NARROW: the new tag is readable by " +
		"the people common to ALL its sources. If your content is a verbatim copy of one source, do not use this — copy " +
		"that source's whole region instead. If the validator rejects it, your content drew on something outside the " +
		"named sources: name every source it actually uses, or do not disclose it."
}

func (*deriveTagTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object","additionalProperties":false,
		"properties":{
			"derived_from":{"type":"array","items":{"type":"string"},
				"description":"The ids of the pt-untrusted regions your synthesized content draws on. At least one."},
			"content":{"type":"string",
				"description":"The exact synthesized text to tag. The validator checks it introduces nothing beyond the named sources, and it is what you paste back verbatim."}
		},
		"required":["derived_from","content"]
	}`)
}

type deriveTagArgs struct {
	DerivedFrom []string `json:"derived_from"`
	Content     string   `json:"content"`
}

func (t *deriveTagTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	if t.cfg.Mint == nil {
		return tool.Result{Content: "derive_tag: provenance tagging is not enabled for this session.", IsError: true, Trusted: true}, nil
	}
	var args deriveTagArgs
	if res, ok := tool.ParseArgs(raw, &args, "derive_tag", `{"derived_from":["pt_1"]}`); !ok {
		return res, nil
	}
	if len(args.DerivedFrom) == 0 {
		return tool.Result{Content: "derive_tag: derived_from must name at least one source tag — a tag with no sources would name its own audience, which only the platform may do.", IsError: true, Trusted: true}, nil
	}
	if len(args.DerivedFrom) > maxDeriveInputs {
		return tool.Result{Content: fmt.Sprintf("derive_tag: at most %d sources.", maxDeriveInputs), IsError: true, Trusted: true}, nil
	}
	content := strings.TrimSpace(args.Content)
	if content == "" {
		return tool.Result{Content: "derive_tag: content is required — pass the exact synthesized text to tag, so the validator can check it and you can paste it back.", IsError: true, Trusted: true}, nil
	}
	// Mint access-checks the inputs, validates that content introduces nothing
	// beyond them, and stores content under the new tag. Any failure is surfaced,
	// not swallowed: a rejected derivation must reach the model so it does not
	// try to disclose content the platform would not vouch for.
	id, err := t.cfg.Mint(ctx, args.DerivedFrom, content)
	if err != nil {
		return tool.Result{Content: "derive_tag: " + err.Error(), IsError: true, Trusted: true}, nil
	}
	// The nonce is generated NOW — after content is fixed — so content cannot
	// contain it and cannot forge the boundary. Hand back the ready-to-paste
	// region; egress verifies the pasted content against what was just stored, so
	// the model cannot alter it after the fact.
	region := toolenvelope.WrapPt(content, toolenvelope.NewNonce(), id)
	return tool.Result{
		Content: fmt.Sprintf("Validated and tagged as %s (readable by everyone common to [%s]). Insert this into your "+
			"reply EXACTLY, verbatim:\n%s", id, strings.Join(args.DerivedFrom, ", "), region),
		Trusted: true,
	}, nil
}
