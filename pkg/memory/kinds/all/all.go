// Package all is a convenience: blank-importing it from a binary triggers
// the init() registration of every memory Kind. internal/cmd/runner (and any other
// binary that wants the framework live) imports it once.
package all

import (
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifactrevision"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/auditkey"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/channel_msg_ref"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/contentguardaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/goalevent"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/kgingestion"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/label"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/lineage"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentthread"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	// A kind omitted from this list is unregistered in the OPERATOR — which
	// serves the memory API and decides whether a Put is storable — so every
	// write of it is answered 400 "unknown Kind"; parked_prompt shipped that
	// way. all_test.go's TestAllKindsRegistered_IncludesLineageAndDispatchSnapshot
	// now fails on any omission.
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/preferenceaccess"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/preferencewrite"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/scopeaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/systemprompt"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/toolchainaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/toolguardaudit"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewparams"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
)
