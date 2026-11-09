package plan

import (
	"fmt"
	"strings"
)

// Requirement is one thing a plan must satisfy.
type Requirement interface {
	Check(*Plan) error
	String() string
}

type reqFunc struct {
	name  string
	check func(*Plan) error
}

func (r reqFunc) Check(p *Plan) error { return r.check(p) }
func (r reqFunc) String() string      { return r.name }

// Require checks every requirement and reports all the failures together
// with the plan.
//
// All of them, not the first. Fixing one requirement at a time against a
// query that breaks three means three round trips through CI, and the plan
// is printed because a failure that says only "the plan changed" is one
// people switch off.
func Require(p *Plan, reqs ...Requirement) error {
	var failed []string
	for _, r := range reqs {
		if err := r.Check(p); err != nil {
			failed = append(failed, "  "+err.Error())
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("the plan does not meet %d of %d requirements:\n%s\n\n%s",
		len(failed), len(reqs), strings.Join(failed, "\n"), p.String())
}

// NoSeqScanOn fails if a table is read end to end.
//
// Named per table rather than banned outright, because a sequential scan of
// a small lookup table is the right plan and forbidding it everywhere makes
// the rule something to work around.
func NoSeqScanOn(tables ...string) Requirement {
	return reqFunc{
		name: "no sequential scan on " + strings.Join(tables, ", "),
		check: func(p *Plan) error {
			var found []string
			for _, s := range p.SeqScans() {
				for _, t := range tables {
					if s == t {
						found = append(found, s)
					}
				}
			}
			if len(found) > 0 {
				return fmt.Errorf("sequential scan on %s", strings.Join(found, ", "))
			}
			return nil
		},
	}
}

// UsesIndex fails unless the named index is used.
//
// Pinning the index by name is stricter than checking that some index was
// used, and that is the point: a query that silently moves onto a different
// index has had its plan change, and finding out at that moment is cheaper
// than finding out when the other index is dropped.
func UsesIndex(name string) Requirement {
	return reqFunc{
		name: "uses index " + name,
		check: func(p *Plan) error {
			for _, i := range p.Indexes() {
				if i == name {
					return nil
				}
			}
			used := p.Indexes()
			if len(used) == 0 {
				return fmt.Errorf("%s is not used, and neither is any other index", name)
			}
			return fmt.Errorf("%s is not used; the plan uses %s", name, strings.Join(used, ", "))
		},
	}
}

// MaxRowsRead caps the rows every scan produces, counting a node inside a
// nested loop once per loop.
//
// The requirement that catches the most. A query returning twenty rows that
// reads two hundred thousand is doing the index's work by hand, and it
// passes a timing test until the table grows.
func MaxRowsRead(n float64) Requirement {
	return reqFunc{
		name: fmt.Sprintf("reads at most %.0f rows", n),
		check: func(p *Plan) error {
			if got := p.RowsRead(); got > n {
				return fmt.Errorf("reads %.0f rows, over the %.0f allowed", got, n)
			}
			return nil
		},
	}
}

// MaxRowsDiscarded caps the rows read and then thrown away by a filter.
func MaxRowsDiscarded(n float64) Requirement {
	return reqFunc{
		name: fmt.Sprintf("discards at most %.0f rows", n),
		check: func(p *Plan) error {
			if got := p.RowsDiscarded(); got > n {
				return fmt.Errorf("reads and discards %.0f rows, over the %.0f allowed", got, n)
			}
			return nil
		},
	}
}

// NoDiskSort fails if a sort spilled to disk.
func NoDiskSort() Requirement {
	return reqFunc{
		name: "no sort spills to disk",
		check: func(p *Plan) error {
			sorts := p.DiskSorts()
			if len(sorts) == 0 {
				return nil
			}
			var parts []string
			for _, s := range sorts {
				parts = append(parts, fmt.Sprintf("%v used %dkB on disk", s.SortKey, s.SortSpaceUsed))
			}
			return fmt.Errorf("sort spilled: %s", strings.Join(parts, "; "))
		},
	}
}

// NoSort fails if the plan sorts at all, which is what an index providing
// the order already is for.
func NoSort() Requirement {
	return reqFunc{
		name: "no sort node",
		check: func(p *Plan) error {
			sorts := p.Nodes("Sort")
			if len(sorts) == 0 {
				return nil
			}
			return fmt.Errorf("sorts on %v, so the index is not providing the order", sorts[0].SortKey)
		},
	}
}

// MaxLoops caps how many times any node runs, which is how an N+1 shows
// itself in a plan.
func MaxLoops(n float64) Requirement {
	return reqFunc{
		name: fmt.Sprintf("no node runs more than %.0f times", n),
		check: func(p *Plan) error {
			if got := p.MaxLoops(); got > n {
				return fmt.Errorf("a node runs %.0f times, which is a query per row rather than a join", got)
			}
			return nil
		},
	}
}

// NoNodeType fails if a node type appears, for the cases where the node
// itself is the problem.
func NoNodeType(types ...string) Requirement {
	return reqFunc{
		name: "no " + strings.Join(types, " or "),
		check: func(p *Plan) error {
			for _, t := range types {
				if n := p.Nodes(t); len(n) > 0 {
					return fmt.Errorf("the plan contains a %s", t)
				}
			}
			return nil
		},
	}
}
