package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// bannedLogAttrKeys are structured-log attribute keys whose values carry
// verbatim caller or agent content: utterance text, DTMF digits, or a whole
// provider/event payload that embeds them. They are allowed at debug only.
var bannedLogAttrKeys = map[string]bool{
	"text":       true,
	"data":       true,
	"raw":        true,
	"content":    true,
	"transcript": true,
	"message":    true,
	"body":       true,
	"payload":    true,
	"digit":      true,
	"digits":     true,
}

// defaultLevelLogMethods are the slog methods that emit at or above the
// default LOG_LEVEL of info, i.e. the ones that reach a production log sink
// without an operator opting in.
var defaultLevelLogMethods = map[string]bool{
	"Info":  true,
	"Warn":  true,
	"Error": true,
}

// scannedLogDirs are the trees that handle transcript, DTMF and event payload
// content. A regex cannot do this job: `.Info("event", "type", string(e.Type),
// "data", e.Data)` hides its banned key behind a nested call, so any pattern
// anchored on a single argument list misses it. The AST sees every argument.
var scannedLogDirs = []string{
	"internal/stt",
	"internal/agent",
	"internal/api",
	"internal/events",
	"cmd/voiceblender",
}

// TestNoVerbatimPayloadAttrsAboveDebug enforces the logging policy: verbatim
// utterance text, DTMF digits, and whole provider or event payloads never
// appear at info or above. They remain available at debug.
func TestNoVerbatimPayloadAttrsAboveDebug(t *testing.T) {
	root := repoRoot(t)
	var offenders []string

	for _, dir := range scannedLogDirs {
		offenders = append(offenders, scanForBannedLogAttrs(t, filepath.Join(root, dir))...)
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d log call(s) pass a verbatim-content attribute at info level or above; demote them to Debug:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func scanForBannedLogAttrs(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
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
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !defaultLevelLogMethods[sel.Sel.Name] {
				return true
			}
			// args[0] is the slog message, never an attribute key.
			if len(call.Args) < 2 {
				return true
			}
			for _, arg := range call.Args[1:] {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				key, uerr := strconv.Unquote(lit.Value)
				if uerr != nil || !bannedLogAttrKeys[key] {
					continue
				}
				pos := fset.Position(lit.Pos())
				rel, rerr := filepath.Rel(repoRoot(t), pos.Filename)
				if rerr != nil {
					rel = pos.Filename
				}
				out = append(out, rel+":"+strconv.Itoa(pos.Line)+" ."+sel.Sel.Name+`(… "`+key+`" …)`)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}
