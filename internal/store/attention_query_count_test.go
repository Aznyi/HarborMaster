package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The attention page's query budget, pinned to an exact number.
//
// # Why this is counted from the source
//
// The database is opened with a hard-coded driver name, so a test cannot slip a
// counting driver underneath it without changing production code to accept one
// -- and adding an injection point purely so a test can watch it is a worse
// trade than counting the call sites. What follows parses this package and
// counts the statements the gather phase can issue, which is the number that
// regresses when somebody adds a lookup.
//
// It counts CALL SITES reachable from Attention, not executions. Those are the
// same thing here only because no gatherer loops: each issues its statements
// once for the whole page, with the page's identifiers bound in one IN list.
// TestAttentionCostDoesNotGrowWithPageSize is the empirical half and proves the
// no-loop property; this is the exact-number half.
//
// # The two numbers, and why they differ
//
// TEN gathers are registered in Attention. ELEVEN statements are issued,
// because gatherPreserved chains into gatherRolledBack -- parked originals and
// rolled-back replacements are separate records and have been read separately
// since long before any of this. The distinction matters when reading the
// batch history: C3A added a tenth GATHER, C3C briefly made image intelligence
// cost two STATEMENTS, and C3G folded it back to one.
const (
	attentionGathers    = 10
	attentionStatements = 11
)

func TestTheAttentionPageIssuesExactlyItsBudget(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "attention_repository.go", nil, 0)
	if err != nil {
		t.Fatalf("parse attention_repository.go: %v", err)
	}

	statements := 0
	perGatherer := map[string]int{}
	registered := 0

	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if !ok || decl.Name == nil {
			return true
		}
		name := decl.Name.Name

		// The registered list lives inside Attention as a slice of closures,
		// one per gather. Counting the calls it makes is counting the list.
		if name == "Attention" {
			ast.Inspect(decl, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok &&
					strings.HasPrefix(selector.Sel.Name, "gather") {
					registered++
				}
				return true
			})
			return true
		}

		if !strings.HasPrefix(name, "gather") {
			return true
		}
		ast.Inspect(decl, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "QueryContext", "QueryRowContext", "ExecContext":
				statements++
				perGatherer[name]++
			}
			return true
		})
		return true
	})

	if registered != attentionGathers {
		t.Errorf("Attention registers %d gathers, want %d", registered, attentionGathers)
	}
	if statements != attentionStatements {
		t.Errorf("the gather phase issues %d statements, want %d.\n\n"+
			"per gatherer: %v\n\n"+
			"A container list must cost a FIXED number of queries whatever the "+
			"page size, and that number is the budget. If a new statement is "+
			"genuinely required, raise attentionStatements DELIBERATELY and say "+
			"why -- do not adjust it to make a build pass. Rendering a list is "+
			"not a reason to talk to the database more.",
			statements, attentionStatements, perGatherer)
	}

	// Non-vacuity: the parse actually found the gatherers rather than matching
	// nothing and passing on two zeroes.
	if len(perGatherer) < attentionGathers {
		t.Fatalf("only %d gatherers were found; the parse is not seeing this "+
			"file properly: %v", len(perGatherer), perGatherer)
	}

	// Image intelligence is ONE statement. It was two between C3C and C3G,
	// because the shared full-record projection could not carry the container
	// id and a separate mapping read was needed first.
	if got := perGatherer["gatherImageIntel"]; got != 1 {
		t.Errorf("gatherImageIntel issues %d statements, want 1 -- it joins "+
			"through containers.image_canonical and needs no mapping read", got)
	}
}
