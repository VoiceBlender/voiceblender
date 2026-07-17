package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// reasonProducers are functions whose returned string literals become a
// cdr.reason. Their results reach publishDisconnect through a local
// variable, so scanning the call sites alone cannot see them.
var reasonProducers = map[string]bool{
	"inviteFailureReason": true,
	"classifyWSReason":    true,
	"classifyWSDialError": true,
	"leaveReasonString":   true,
}

// reasonConsts are constants published as a cdr.reason from another package
// through a callback, which likewise hides them from the call sites.
var reasonConsts = map[string]bool{
	"legPanicReason": true,
}

// reasonSinks map a function name to the index of its argument that becomes
// a cdr.reason. publishDisconnect publishes it directly; the others forward
// it across a function boundary, so a literal at their call sites never
// appears next to publishDisconnect: Transport.cleanup stores the argument
// as the close reason that liveKitConn.watch later reads via CloseReason()
// and publishes, and disconnectParticipantLeg passes it straight through.
var reasonSinks = map[string]int{
	"publishDisconnect":        1,
	"cleanup":                  1,
	"disconnectParticipantLeg": 1,
}

// collectPublishedReasons walks the non-test sources under root and returns
// every reason string literal it can attribute to the code, mapped to the
// file it came from. It reads three shapes: a literal passed to a
// reasonSinks function, a literal assigned to the variable such a call
// passes, and a literal returned by a reasonProducers function or bound to
// a reasonConsts constant.
//
// It deliberately under-reports rather than guesses: reasons composed at
// runtime (fmt.Sprintf("sip_%d", …), "ice_"+state) and reasons supplied by
// the caller in a request body have no literal to find. So this proves every
// reason it *does* find is documented; it cannot prove the reverse.
func collectPublishedReasons(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if reasonProducers[node.Name.Name] {
					for _, lit := range returnedStringLits(node) {
						found[lit] = path
					}
				}
				for _, lit := range publishedInFunc(node) {
					found[lit] = path
				}
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if !reasonConsts[name.Name] || i >= len(node.Values) {
						continue
					}
					if lit, ok := stringLit(node.Values[i]); ok {
						found[lit] = path
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// publishedInFunc returns the reason literals a single function hands to a
// reasonSinks function, following one level of local variable assignment.
func publishedInFunc(fn *ast.FuncDecl) []string {
	var lits, vars []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		idx, ok := reasonSinks[sel.Sel.Name]
		if !ok || idx >= len(call.Args) {
			return true
		}
		if lit, ok := stringLit(call.Args[idx]); ok {
			lits = append(lits, lit)
			return true
		}
		if id, ok := call.Args[idx].(*ast.Ident); ok {
			vars = append(vars, id.Name)
		}
		return true
	})
	for _, name := range vars {
		lits = append(lits, assignedStringLits(fn, name)...)
	}
	return lits
}

// assignedStringLits returns the string literals assigned to name in fn.
func assignedStringLits(fn *ast.FuncDecl, name string) []string {
	var lits []string
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name || i >= len(as.Rhs) {
				continue
			}
			if lit, ok := stringLit(as.Rhs[i]); ok {
				lits = append(lits, lit)
			}
		}
		return true
	})
	return lits
}

// returnedStringLits returns the string literals fn can return.
func returnedStringLits(fn *ast.FuncDecl) []string {
	var lits []string
	ast.Inspect(fn, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, r := range ret.Results {
			if lit, ok := stringLit(r); ok {
				lits = append(lits, lit)
			}
		}
		return true
	})
	return lits
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// findSchemasByDescription returns every mapping node carrying exactly the
// given description, wherever it sits in the document.
func findSchemasByDescription(n *yaml.Node, desc string) []*yaml.Node {
	var out []*yaml.Node
	if n.Kind == yaml.MappingNode {
		if d := mappingValue(n, "description"); d != nil && d.Value == desc {
			out = append(out, n)
		}
	}
	for _, c := range n.Content {
		out = append(out, findSchemasByDescription(c, desc)...)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// TestKnownDisconnectReasonsTest guards the documented list against the code
// that publishes reasons. mixer_panic reaches the CDR only through a room
// callback, so a list edited by hand drops it without anything noticing.
func TestKnownDisconnectReasonsTest(t *testing.T) {
	known := map[string]bool{}
	for _, r := range KnownDisconnectReasons {
		if known[r] {
			t.Errorf("KnownDisconnectReasons lists %q twice", r)
		}
		known[r] = true
	}

	published := collectPublishedReasons(t, repoRoot(t))
	if len(published) < 20 {
		t.Fatalf("found only %d reasons in the source; the scan is broken, not the list", len(published))
	}
	for reason, path := range published {
		if !known[reason] {
			t.Errorf("%s publishes reason %q, missing from KnownDisconnectReasons", path, reason)
		}
	}
	for _, r := range []string{"mixer_panic", "room_deleted", "transfer_completed", "bad_answer", "challenged"} {
		if !known[r] {
			t.Errorf("KnownDisconnectReasons is missing %q", r)
		}
	}
}

// TestCDRReasonSpecIsOpen asserts the generated spec, not the Go list: the
// enum must be gone, and the description must carry the known values so the
// documentation survives opening the field.
func TestCDRReasonSpecIsOpen(t *testing.T) {
	specPath := filepath.Join(repoRoot(t), "openapi.yaml")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	desc := disconnectReasonDescription()
	schemas := findSchemasByDescription(&doc, desc)
	if len(schemas) == 0 {
		t.Fatalf("openapi.yaml does not carry the cdr.reason description; run go generate ./internal/api/")
	}
	for _, s := range schemas {
		if enum := mappingValue(s, "enum"); enum != nil {
			t.Errorf("cdr.reason is still constrained by an enum of %d values", len(enum.Content))
		}
	}
	for _, r := range KnownDisconnectReasons {
		if !strings.Contains(desc, r) {
			t.Errorf("description omits known reason %q", r)
		}
	}
	if !strings.Contains(desc, "sip_{code}") {
		t.Error("description no longer documents the sip_{code} open set")
	}
}
