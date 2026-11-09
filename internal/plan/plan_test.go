package plan_test

import (
	"strings"
	"testing"

	"github.com/usmanvirk/query-plans/internal/plan"
)

// A real EXPLAIN (ANALYZE, FORMAT JSON) result, trimmed to the fields this
// package reads. Kept as a literal rather than generated, so the parser is
// tested against the shape Postgres actually emits.
const seqScanJSON = `[{
  "Plan": {
    "Node Type": "Limit",
    "Total Cost": 4621.0,
    "Plan Rows": 20,
    "Actual Rows": 20,
    "Actual Loops": 1,
    "Actual Total Time": 7.81,
    "Plans": [{
      "Node Type": "Sort",
      "Sort Key": ["price_cents"],
      "Sort Method": "external merge",
      "Sort Space Used": 4096,
      "Sort Space Type": "Disk",
      "Actual Rows": 20,
      "Actual Loops": 1,
      "Plans": [{
        "Node Type": "Seq Scan",
        "Relation Name": "listings",
        "Alias": "listings",
        "Filter": "((status = 'available') AND (category_id = 7))",
        "Rows Removed by Filter": 198990,
        "Actual Rows": 1010,
        "Actual Loops": 1,
        "Total Cost": 4500.0,
        "Plan Rows": 1010
      }]
    }]
  },
  "Planning Time": 0.2,
  "Execution Time": 7.81
}]`

const indexScanJSON = `[{
  "Plan": {
    "Node Type": "Limit",
    "Actual Rows": 20,
    "Actual Loops": 1,
    "Plans": [{
      "Node Type": "Index Scan",
      "Relation Name": "listings",
      "Index Name": "listings_browse_idx",
      "Index Cond": "((status = 'available') AND (category_id = 7))",
      "Actual Rows": 20,
      "Actual Loops": 1,
      "Total Cost": 8.4,
      "Plan Rows": 20
    }]
  },
  "Planning Time": 0.1,
  "Execution Time": 0.1
}]`

// A nested loop that runs once per outer row, which is what an N+1 looks
// like from the planner's side.
const nestedLoopJSON = `[{
  "Plan": {
    "Node Type": "Nested Loop",
    "Actual Rows": 100,
    "Actual Loops": 1,
    "Plans": [
      {"Node Type": "Index Scan", "Relation Name": "listings",
       "Index Name": "listings_seller_id_idx", "Actual Rows": 100, "Actual Loops": 1},
      {"Node Type": "Index Scan", "Relation Name": "events",
       "Index Name": "events_listing_id_idx",
       "Actual Rows": 4, "Actual Loops": 100, "Rows Removed by Filter": 3}
    ]
  },
  "Execution Time": 12.0
}]`

func parse(t *testing.T, raw string) *plan.Plan {
	t.Helper()
	p, err := plan.Parse("SELECT 1", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseReadsTheTree(t *testing.T) {
	p := parse(t, seqScanJSON)
	if p.Root.NodeType != "Limit" {
		t.Fatalf("root is %q", p.Root.NodeType)
	}
	if p.ExecutionTime != 7.81 {
		t.Fatalf("execution time is %v", p.ExecutionTime)
	}
	if got := p.SeqScans(); len(got) != 1 || got[0] != "listings" {
		t.Fatalf("seq scans are %v", got)
	}
	if len(p.Nodes("Sort")) != 1 {
		t.Fatal("the sort node was not found")
	}
}

func TestParseRefusesRubbish(t *testing.T) {
	for _, raw := range []string{"", "[]", "not json", "{}"} {
		if _, err := plan.Parse("q", []byte(raw)); err == nil {
			t.Errorf("%q parsed as a plan", raw)
		}
	}
}

func TestRowsDiscarded(t *testing.T) {
	if got := parse(t, seqScanJSON).RowsDiscarded(); got != 198990 {
		t.Fatalf("discarded %v, want 198990", got)
	}
	if got := parse(t, indexScanJSON).RowsDiscarded(); got != 0 {
		t.Fatalf("an index scan discarded %v", got)
	}
}

// TestRowsDiscardedCountsEveryLoop. A node inside a nested loop discarding
// three rows a time, a hundred times, has discarded three hundred, and
// counting it once is how an N+1 hides.
func TestRowsDiscardedCountsEveryLoop(t *testing.T) {
	if got := parse(t, nestedLoopJSON).RowsDiscarded(); got != 300 {
		t.Fatalf("discarded %v, want 300: three rows across a hundred loops", got)
	}
}

func TestRowsReadCountsEveryLoop(t *testing.T) {
	// 100 from the outer scan, plus 4 rows across 100 loops on the inner.
	if got := parse(t, nestedLoopJSON).RowsRead(); got != 500 {
		t.Fatalf("read %v, want 500", got)
	}
}

func TestMaxLoops(t *testing.T) {
	if got := parse(t, nestedLoopJSON).MaxLoops(); got != 100 {
		t.Fatalf("max loops is %v, want 100", got)
	}
	if got := parse(t, indexScanJSON).MaxLoops(); got != 1 {
		t.Fatalf("max loops is %v, want 1", got)
	}
}

func TestDiskSorts(t *testing.T) {
	if got := parse(t, seqScanJSON).DiskSorts(); len(got) != 1 {
		t.Fatalf("%d disk sorts, want 1", len(got))
	}
	if got := parse(t, indexScanJSON).DiskSorts(); len(got) != 0 {
		t.Fatalf("%d disk sorts in a plan with no sort", len(got))
	}
}

func TestIndexes(t *testing.T) {
	got := parse(t, indexScanJSON).Indexes()
	if len(got) != 1 || got[0] != "listings_browse_idx" {
		t.Fatalf("indexes are %v", got)
	}
}

func TestRequirementsPassOnAGoodPlan(t *testing.T) {
	p := parse(t, indexScanJSON)
	if err := plan.Require(p,
		plan.NoSeqScanOn("listings"),
		plan.UsesIndex("listings_browse_idx"),
		plan.NoSort(),
		plan.NoDiskSort(),
		plan.MaxRowsRead(50),
		plan.MaxRowsDiscarded(0),
		plan.MaxLoops(1),
	); err != nil {
		t.Fatal(err)
	}
}

// TestRequirementsReportEveryFailure, not the first. Fixing one at a time
// against a query that breaks three means three round trips through CI.
func TestRequirementsReportEveryFailure(t *testing.T) {
	p := parse(t, seqScanJSON)
	err := plan.Require(p,
		plan.NoSeqScanOn("listings"),
		plan.UsesIndex("listings_browse_idx"),
		plan.NoSort(),
		plan.NoDiskSort(),
		plan.MaxRowsRead(50),
	)
	if err == nil {
		t.Fatal("a sequential scan with a disk sort satisfied every requirement")
	}
	msg := err.Error()
	for _, want := range []string{
		"sequential scan on listings",
		"listings_browse_idx is not used",
		"sorts on",
		"sort spilled",
		"over the 50 allowed",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "5 of 5") {
		t.Errorf("the failure does not say how many requirements failed:\n%s", msg)
	}
}

// TestTheFailureIncludesThePlan. A test that says only "the plan changed"
// and not how is one people switch off.
func TestTheFailureIncludesThePlan(t *testing.T) {
	p := parse(t, seqScanJSON)
	err := plan.Require(p, plan.NoSeqScanOn("listings"))
	if err == nil {
		t.Fatal("no failure")
	}
	msg := err.Error()
	for _, want := range []string{"Seq Scan on listings", "discarded=198990", "Sort"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure does not show %q:\n%s", want, msg)
		}
	}
}

func TestNoSeqScanIsPerTable(t *testing.T) {
	p := parse(t, seqScanJSON)
	// A sequential scan of a small lookup table is the right plan, so the
	// rule names the tables it cares about rather than banning the node.
	if err := plan.Require(p, plan.NoSeqScanOn("sellers")); err != nil {
		t.Fatalf("a scan of listings failed a rule about sellers: %v", err)
	}
}

func TestUsesIndexSaysWhichIndexWasUsedInstead(t *testing.T) {
	p := parse(t, indexScanJSON)
	err := plan.Require(p, plan.UsesIndex("some_other_idx"))
	if err == nil {
		t.Fatal("no failure")
	}
	if !strings.Contains(err.Error(), "listings_browse_idx") {
		t.Fatalf("the failure does not name the index that was used:\n%s", err)
	}
}

func TestMaxLoopsCatchesAQueryPerRow(t *testing.T) {
	p := parse(t, nestedLoopJSON)
	if err := plan.Require(p, plan.MaxLoops(10)); err == nil {
		t.Fatal("a node running a hundred times passed a limit of ten")
	}
}

func TestNoNodeType(t *testing.T) {
	p := parse(t, seqScanJSON)
	if err := plan.Require(p, plan.NoNodeType("Sort")); err == nil {
		t.Fatal("a plan containing a Sort passed NoNodeType(Sort)")
	}
	if err := plan.Require(p, plan.NoNodeType("Hash Join")); err != nil {
		t.Fatalf("a plan with no hash join failed: %v", err)
	}
}

func TestStringRendersTheTree(t *testing.T) {
	s := parse(t, seqScanJSON).String()
	for _, want := range []string{"Limit", "Sort", "Seq Scan on listings", "execution 7.81ms"} {
		if !strings.Contains(s, want) {
			t.Errorf("the rendered plan is missing %q:\n%s", want, s)
		}
	}
}

func TestWalkCanStopEarly(t *testing.T) {
	p := parse(t, seqScanJSON)
	n := 0
	p.Walk(func(plan.Node) bool { n++; return false })
	if n != 1 {
		t.Fatalf("returning false visited %d nodes, want 1", n)
	}
}
