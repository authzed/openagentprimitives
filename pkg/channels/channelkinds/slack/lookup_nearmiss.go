// pkg/channels/channelkinds/slack/lookup_nearmiss.go
//
// Third pass of the name lookup: when neither exact pass matched, collect
// near-miss candidates for the agent to judge. A user qualifies when their
// surname (last name token) matches the requested surname — nicknames vary
// the given name (Frederick→Freddie), rarely the surname — or when their full
// name is within edit distance of the request. The code never auto-picks a
// candidate: the agent has the conversational context to decide whether
// "Freddie Cavendish" is the "Frederick Cavendish" it was asked about.
package slack

import (
	"sort"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/xrash/smetrics"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// nearMissMaxCandidates bounds what a single lookup can put into the
// model's context; the closest five is plenty for a judgement call.
const nearMissMaxCandidates = 5

// nearMissCandidates scans the workspace snapshot for near-miss matches
// of want (already lowercased/trimmed by lookupByName). Deleted and bot
// accounts are excluded — a wrong mention pings someone, so the fuzzy
// pass is stricter about who it offers than the exact passes need to be.
// Results are ranked closest-first (ties by user ID for determinism) and
// capped at nearMissMaxCandidates.
func nearMissCandidates(users []slackapi.User, want string) []channelkinds.MentionCandidate {
	if want == "" {
		return nil
	}
	wantSurname := lastNameToken(want)
	maxDist := nearMissThreshold(want)

	type scored struct {
		user slackapi.User
		dist int
	}
	cands := make([]scored, 0, nearMissMaxCandidates)
	for _, u := range users {
		if u.Deleted || u.IsBot {
			continue
		}
		dist := -1
		surname := false
		for _, name := range []string{
			strings.ToLower(strings.TrimSpace(u.Profile.DisplayName)),
			strings.ToLower(strings.TrimSpace(u.Profile.RealName)),
		} {
			if name == "" {
				continue
			}
			if d := smetrics.WagnerFischer(want, name, 1, 1, 1); dist < 0 || d < dist {
				dist = d
			}
			if lastNameToken(name) == wantSurname {
				surname = true
			}
		}
		if dist < 0 { // profile carries no usable name
			continue
		}
		if !surname && dist > maxDist {
			continue
		}
		cands = append(cands, scored{user: u, dist: dist})
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].dist != cands[j].dist {
			return cands[i].dist < cands[j].dist
		}
		return cands[i].user.ID < cands[j].user.ID
	})
	if len(cands) > nearMissMaxCandidates {
		cands = cands[:nearMissMaxCandidates]
	}

	out := make([]channelkinds.MentionCandidate, 0, len(cands))
	for i := range cands {
		out = append(out, channelkinds.MentionCandidate{
			ExternalID:  cands[i].user.ID,
			DisplayName: preferredDisplayName(&cands[i].user),
			AccountType: mentionStanding(&cands[i].user),
		})
	}
	return out
}

func lastNameToken(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// nearMissThreshold scales the acceptable edit distance with the length
// of the requested name — roughly one edit per five characters, minimum
// two — so short names don't fuzzy-match half the directory while longer
// ones tolerate a nickname's worth of drift.
func nearMissThreshold(want string) int {
	t := (len([]rune(want)) + 4) / 5
	if t < 2 {
		t = 2
	}
	return t
}
