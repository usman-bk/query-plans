// Package plan reads what Postgres says it will do with a query, so a test
// can object to it.
//
// The usual way to stop a query regressing is to measure how long it takes,
// and it does not work. Timing on a laptop, on a shared runner and on the
// production box are three different numbers, so the threshold is either so
// loose it catches nothing or so tight it fails on a busy afternoon, and
// either way somebody deletes the test. Worse, a query that is fast on
// 10,000 rows and quadratic above a million passes every time until the day
// it does not.
//
// The plan does not have that problem. A sequential scan over a million rows
// is a sequential scan whatever the machine is doing, and it is visible
// before the query is slow enough for anybody to notice.
package plan

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Node is one step of the plan. The fields are the ones worth asserting on;
// EXPLAIN emits several dozen more.
type Node struct {
	NodeType      string `json:"Node Type"`
	RelationName  string `json:"Relation Name"`
	Alias         string `json:"Alias"`
	IndexName     string `json:"Index Name"`
	ScanDirection string `json:"Scan Direction"`
	JoinType      string `json:"Join Type"`

	StartupCost float64 `json:"Startup Cost"`
	TotalCost   float64 `json:"Total Cost"`
	PlanRows    int64   `json:"Plan Rows"`
	PlanWidth   int64   `json:"Plan Width"`

	// Set only by EXPLAIN ANALYZE.
	ActualRows      float64 `json:"Actual Rows"`
	ActualLoops     float64 `json:"Actual Loops"`
	ActualTotalTime float64 `json:"Actual Total Time"`

	// Rows the filter threw away after reading them, which is the clearest
	// single sign that an index is missing or the wrong shape.
	RowsRemovedByFilter       float64 `json:"Rows Removed by Filter"`
	RowsRemovedByIndexRecheck float64 `json:"Rows Removed by Index Recheck"`

	Filter        string   `json:"Filter"`
	IndexCond     string   `json:"Index Cond"`
	SortKey       []string `json:"Sort Key"`
	SortMethod    string   `json:"Sort Method"`
	SortSpaceUsed int64    `json:"Sort Space Used"`
	SortSpaceType string   `json:"Sort Space Type"`

	Plans []Node `json:"Plans"`
}

// Plan is the whole tree, plus what the planner was asked.
type Plan struct {
	Root          Node
	ExecutionTime float64
	PlanningTime  float64
	Query         string
}

type explainRow struct {
	Plan          Node    `json:"Plan"`
	ExecutionTime float64 `json:"Execution Time"`
	PlanningTime  float64 `json:"Planning Time"`
}

// Parse reads the output of EXPLAIN (FORMAT JSON).
func Parse(query string, raw []byte) (*Plan, error) {
	var rows []explainRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("parsing explain output: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("explain returned nothing")
	}
	return &Plan{
		Root:          rows[0].Plan,
		ExecutionTime: rows[0].ExecutionTime,
		PlanningTime:  rows[0].PlanningTime,
		Query:         query,
	}, nil
}

// Walk visits every node, parents before children.
func (p *Plan) Walk(fn func(Node) bool) {
	var visit func(Node) bool
	visit = func(n Node) bool {
		if !fn(n) {
			return false
		}
		for _, c := range n.Plans {
			if !visit(c) {
				return false
			}
		}
		return true
	}
	visit(p.Root)
}

// Nodes returns every node of a type, such as "Seq Scan" or "Sort".
func (p *Plan) Nodes(nodeType string) []Node {
	var out []Node
	p.Walk(func(n Node) bool {
		if n.NodeType == nodeType {
			out = append(out, n)
		}
		return true
	})
	return out
}

// Indexes lists every index the plan uses.
func (p *Plan) Indexes() []string {
	var out []string
	seen := map[string]bool{}
	p.Walk(func(n Node) bool {
		if n.IndexName != "" && !seen[n.IndexName] {
			seen[n.IndexName] = true
			out = append(out, n.IndexName)
		}
		return true
	})
	return out
}

// SeqScans lists the tables read end to end.
func (p *Plan) SeqScans() []string {
	var out []string
	p.Walk(func(n Node) bool {
		if n.NodeType == "Seq Scan" && n.RelationName != "" {
			out = append(out, n.RelationName)
		}
		return true
	})
	return out
}

// RowsDiscarded is the total thrown away by filters after being read.
//
// The number worth watching. A query that reads 200,000 rows to return 20
// is doing the work of the missing index by hand, and it will keep passing
// a timing test until the table is large enough for somebody to notice.
func (p *Plan) RowsDiscarded() float64 {
	var total float64
	p.Walk(func(n Node) bool {
		loops := n.ActualLoops
		if loops < 1 {
			loops = 1
		}
		total += (n.RowsRemovedByFilter + n.RowsRemovedByIndexRecheck) * loops
		return true
	})
	return total
}

// RowsRead is the rows actually produced by every scan node, counting a
// node inside a nested loop once per loop.
//
// This is what separates a nested loop that runs twice from one that runs
// twenty thousand times, which is the difference between a join and an N+1
// and is invisible in the node types alone.
func (p *Plan) RowsRead() float64 {
	var total float64
	p.Walk(func(n Node) bool {
		if !strings.HasSuffix(n.NodeType, "Scan") {
			return true
		}
		loops := n.ActualLoops
		if loops < 1 {
			loops = 1
		}
		total += n.ActualRows * loops
		return true
	})
	return total
}

// DiskSorts lists sorts that had to spill, which is the point at which a
// sort stops being cheap.
func (p *Plan) DiskSorts() []Node {
	var out []Node
	p.Walk(func(n Node) bool {
		if n.NodeType == "Sort" && strings.Contains(strings.ToLower(n.SortSpaceType), "disk") {
			out = append(out, n)
		}
		return true
	})
	return out
}

// MaxLoops is the largest loop count in the plan, which is how an N+1 shows
// itself.
func (p *Plan) MaxLoops() float64 {
	var max float64
	p.Walk(func(n Node) bool {
		if n.ActualLoops > max {
			max = n.ActualLoops
		}
		return true
	})
	return max
}

// String renders the tree the way EXPLAIN does, for a failure message
// somebody can act on. A test that says "the plan changed" and does not say
// how is a test people disable.
func (p *Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", strings.TrimSpace(p.Query))
	var render func(Node, int)
	render = func(n Node, depth int) {
		indent := strings.Repeat("  ", depth)
		fmt.Fprintf(&b, "%s-> %s", indent, n.NodeType)
		if n.RelationName != "" {
			fmt.Fprintf(&b, " on %s", n.RelationName)
		}
		if n.IndexName != "" {
			fmt.Fprintf(&b, " using %s", n.IndexName)
		}
		fmt.Fprintf(&b, " (cost=%.1f rows=%d", n.TotalCost, n.PlanRows)
		if n.ActualLoops > 0 {
			fmt.Fprintf(&b, " actual_rows=%.0f loops=%.0f time=%.2fms",
				n.ActualRows, n.ActualLoops, n.ActualTotalTime)
		}
		b.WriteString(")")
		if n.RowsRemovedByFilter > 0 {
			fmt.Fprintf(&b, " discarded=%.0f", n.RowsRemovedByFilter)
		}
		if n.SortMethod != "" {
			fmt.Fprintf(&b, " sort=%s %s", n.SortMethod, n.SortSpaceType)
		}
		b.WriteString("\n")
		for _, c := range n.Plans {
			render(c, depth+1)
		}
	}
	render(p.Root, 0)
	if p.ExecutionTime > 0 {
		fmt.Fprintf(&b, "execution %.2fms, planning %.2fms\n", p.ExecutionTime, p.PlanningTime)
	}
	return b.String()
}
