package restic_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

// Production packages that must never construct forbidden restic subcommands
// as string literals used in argv position zero patterns.
var productionDirs = []string{
	"internal/restic",
	"internal/repositories",
	"internal/mcpserver",
	"internal/config",
	"internal/policy",
	"internal/redaction",
	"internal/audit",
	"cmd/restic-defensive-mcp",
}

func TestProductionHasNoForbiddenSubcommandLiteralsInArgvBuilders(t *testing.T) {
	// Full forbidden command list from production (not a hand-maintained subset).
	forbiddenExact := restic.ForbiddenCommands()

	// Walk production AST; flag string literals that are exactly a forbidden
	// subcommand inside []string composite literals (argv construction).
	root := findModuleRoot(t)
	fset := token.NewFileSet()
	for _, dir := range productionDirs {
		abs := filepath.Join(root, dir)
		entries, err := os.ReadDir(abs)
		if err != nil {
			// optional dirs
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(abs, e.Name())
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				// The denylist itself necessarily contains these literals. Skip
				// only that declaration, not the rest of commands.go.
				if value, ok := n.(*ast.ValueSpec); ok {
					for _, name := range value.Names {
						if name.Name == "forbiddenSubcommands" {
							return false
						}
					}
				}
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				// look for []string{ "backup", ... }
				for _, elt := range cl.Elts {
					bl, ok := elt.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						continue
					}
					val := strings.Trim(bl.Value, `"`)
					for _, bad := range forbiddenExact {
						if val == bad {
							t.Errorf("%s: forbidden restic subcommand literal %q in composite literal", path, bad)
						}
					}
				}
				return true
			})
		}
	}
}

func TestProductionDoesNotImportTestRepositoryHarness(t *testing.T) {
	root := findModuleRoot(t)
	fset := token.NewFileSet()
	for _, dir := range productionDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, dir, entry.Name())
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range file.Imports {
				if strings.Trim(imported.Path.Value, `"`) == "github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo" {
					t.Errorf("%s: production code must not import the mutating test repository harness", path)
				}
			}
		}
	}
}

func TestProductionProcessExecutionIsConfinedToResticRunner(t *testing.T) {
	root := findModuleRoot(t)
	fset := token.NewFileSet()
	for _, dir := range productionDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, dir, entry.Name())
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			execAliases := make(map[string]struct{})
			for _, imported := range file.Imports {
				if strings.Trim(imported.Path.Value, `"`) != "os/exec" {
					continue
				}
				name := "exec"
				if imported.Name != nil {
					name = imported.Name.Name
				}
				execAliases[name] = struct{}{}
			}
			if len(execAliases) == 0 {
				continue
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "Command" && selector.Sel.Name != "CommandContext") {
					return true
				}
				ident, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				if _, isExec := execAliases[ident.Name]; !isExec {
					return true
				}
				if dir != "internal/restic" || entry.Name() != "runner.go" {
					t.Errorf("%s: production process execution must remain confined to internal/restic/runner.go", path)
				}
				return true
			})
		}
	}
}

func TestForbiddenSubcommandsCoversCommonMutations(t *testing.T) {
	// Ensure the exported list (used by AST scan + capabilities) includes core mutations.
	need := []string{"backup", "restore", "forget", "prune", "unlock", "init", "check", "dump", "cat", "tag", "key"}
	forbidden := restic.ForbiddenCommands()
	set := make(map[string]struct{}, len(forbidden))
	for _, s := range forbidden {
		set[s] = struct{}{}
	}
	for _, n := range need {
		if _, ok := set[n]; !ok {
			t.Fatalf("ForbiddenSubcommands missing %q", n)
		}
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
