package memorycmd

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// ShareSource identifies this command's writes — the agentsession
// participant tuple that grants query_memory cross-session reads. Qualified
// (not plain Source) because this package owns more than one command's
// worth of concept (list, get, query, put, delete, share, ...).
//
// Claims deliberately EMPTY. agentsession#participant is also written by
// channelsd's inbound pipeline (TouchInteractParticipant /
// TouchInteractParticipantUser in pkg/authz/spicedb/client.go — typed
// helpers that bypass the guard, same as lineage/attested-identity) as part
// of ordinary session membership, so this command and channelsd genuinely
// share the relation. Claiming it for either would break the other; the
// allow-by-default a relation with no claim gets is exactly what lets it
// stay shared until someone decides a single owner.
//
// Exported for consistency with every other package-owned relsource.Source
// var, even though this package is this name's only writer through the
// guarded path today.
var ShareSource = relsource.Source{Name: "memoryshare"}

func init() {
	relsource.Register(ShareSource)
}

func newMemoryShareCmd(g *apcmd.Globals) *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "share <session> --with <other-session> [--with ...]",
		Short: "Grant another session read access to this session's memories",
		Long: `Write SpiceDB relationships so that the started_by user of each
--with session gains 'interact' permission on the target session.
This enables query_memory cross-session reads for those sessions.

Example:
  oap memory share default/session-a --with default/session-b

This lets the user who started session-b read session-a's memory entries.
Use --revoke to remove the grant.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			withSessions, err := cmd.Flags().GetStringSlice("with")
			if err != nil {
				return err
			}
			if len(withSessions) == 0 {
				return fmt.Errorf("at least one --with session is required")
			}
			return runMemoryShare(cmd, g, args[0], withSessions, revoke)
		},
	}
	cmd.Flags().StringSlice("with", nil, "Session(s) whose started_by user gains read access (namespace/name, repeatable)")
	cmd.Flags().BoolVar(&revoke, "revoke", false, "Remove the grant instead of adding it")
	return cmd
}

func runMemoryShare(cmd *cobra.Command, g *apcmd.Globals, targetSession string, withSessions []string, revoke bool) error {
	ctx := cmd.Context()
	b, err := g.Bundle()
	if err != nil {
		return err
	}

	targetNS, targetName, err := splitSession(targetSession)
	if err != nil {
		return fmt.Errorf("target session: %w", err)
	}

	spiceDBClient, cleanup, err := apspicedb.DialViaPortForward(ctx, b)
	if err != nil {
		return err
	}
	defer cleanup()
	writer := spiceDBClient.Writer(ShareSource)

	out := cmd.OutOrStdout()
	for _, withSess := range withSessions {
		withNS, withName, err := splitSession(withSess)
		if err != nil {
			return fmt.Errorf("--with session: %w", err)
		}

		startedBy, err := lookupStartedBy(ctx, writer, withNS+"/"+withName)
		if err != nil {
			return fmt.Errorf("lookup started_by for %s: %w", withSess, err)
		}
		if startedBy == "" {
			return fmt.Errorf("session %s has no started_by user in SpiceDB", withSess)
		}

		targetID := targetNS + "/" + targetName
		if revoke {
			err = deleteParticipant(ctx, writer, targetID, startedBy)
			if err != nil {
				return fmt.Errorf("revoke grant: %w", err)
			}
			fmt.Fprintf(out, "revoked: user:%s can no longer read agentsession:%s memories\n", startedBy, targetID)
		} else {
			err = writeParticipant(ctx, writer, targetID, startedBy)
			if err != nil {
				return fmt.Errorf("write grant: %w", err)
			}
			fmt.Fprintf(out, "granted: user:%s can now read agentsession:%s memories (via %s started_by)\n", startedBy, targetID, withSess)
		}
	}
	return nil
}

func splitSession(s string) (ns, name string, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%q must be namespace/name", s)
	}
	return parts[0], parts[1], nil
}

func lookupStartedBy(ctx context.Context, w spicedb.RelWriter, sessionID string) (string, error) {
	resp, err := w.LookupSubjects(ctx, &v1.LookupSubjectsRequest{
		Resource: &v1.ObjectReference{
			ObjectType: "agentsession",
			ObjectId:   sessionID,
		},
		Permission:        "interact",
		SubjectObjectType: "user",
		Consistency:       &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	if err != nil {
		return "", err
	}
	for {
		msg, err := resp.Recv()
		if err != nil {
			break
		}
		if msg.Subject != nil && msg.Subject.SubjectObjectId != "" {
			return msg.Subject.SubjectObjectId, nil
		}
	}
	return "", nil
}

func writeParticipant(ctx context.Context, w spicedb.RelWriter, targetSessionID, userID string) error {
	_, err := w.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{
					ObjectType: "agentsession",
					ObjectId:   targetSessionID,
				},
				Relation: "participant",
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{
						ObjectType: "user",
						ObjectId:   userID,
					},
				},
			},
		}},
	})
	return err
}

func deleteParticipant(ctx context.Context, w spicedb.RelWriter, targetSessionID, userID string) error {
	_, err := w.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: targetSessionID,
			OptionalRelation:   "participant",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: userID,
			},
		},
	})
	return err
}
