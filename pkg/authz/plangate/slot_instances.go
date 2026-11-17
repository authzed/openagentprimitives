package plangate

import (
	"fmt"
	"strings"
)

// SlotInstanceConflict is one slot type a plan names more than one distinct
// instance of.
type SlotInstanceConflict struct {
	ResourceType string
	// IDs are the distinct instances named, in plan order.
	IDs []string
}

// RefusalText says why the plan cannot run as written.
func (c SlotInstanceConflict) RefusalText() string {
	return fmt.Sprintf("a run can hold one %s at a time, and this plan names %s; name one %s, or ask for the others in separate goals",
		c.ResourceType, strings.Join(c.IDs, ", "), c.ResourceType)
}

// SlotInstanceConflicts reports every slot type the plan names two or more
// distinct instances of, across all its phases.
//
// A session holds one instance of a slot type, and never rebinds it: a grant
// for a second instance is refused at bind time ("single-occupancy"). A plan
// that names two can therefore be approved but never run as approved — for a
// group goal's standing plan, every run would fail at its first bind. Callers
// that can say so before anyone approves use this; it changes nothing about
// how slots bind. A slot with no ID names no instance and is not counted, and
// the same ID named twice is one instance.
func SlotInstanceConflicts(p Plan) []SlotInstanceConflict {
	var order []string
	ids := map[string][]string{}
	for _, ph := range p.Phases {
		for _, s := range ph.Slots {
			if s.ID == "" {
				continue
			}
			if _, seen := ids[s.Type]; !seen {
				order = append(order, s.Type)
			}
			dup := false
			for _, id := range ids[s.Type] {
				if id == s.ID {
					dup = true
					break
				}
			}
			if !dup {
				ids[s.Type] = append(ids[s.Type], s.ID)
			}
		}
	}
	var out []SlotInstanceConflict
	for _, t := range order {
		if len(ids[t]) > 1 {
			out = append(out, SlotInstanceConflict{ResourceType: t, IDs: ids[t]})
		}
	}
	return out
}
