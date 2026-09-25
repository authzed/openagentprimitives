package bento

import (
	// Blank-import bento's pure components so the `generate` input
	// (and other pure inputs/processors/outputs used in stream YAML)
	// is registered with the default bento environment. Without this,
	// service.NewStreamBuilder().SetYAML(...) fails to infer the input
	// type. Pure components have no external-system dependencies and
	// are safe to always pull in.
	_ "github.com/warpstreamlabs/bento/public/components/pure"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func init() { registry.Register(&Kind{}) }
