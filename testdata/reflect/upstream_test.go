//go:build linux && (amd64 || arm64) && cgo

package reflect_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/main.go.txt
var sandboxMain string

//go:embed testdata/driver.go.txt
var sandboxDriver string

// TestStandardReflect runs the current toolchain's unmodified reflect tests
// inside Sentry. Objects created by those tests live in the guest; the separate
// ixgo/reflect_public_test.go cases exercise host-created object round trips.
func TestStandardReflect(t *testing.T) {
	if os.Getenv("SANDBOX_TEST_LIBRARY") == "" {
		t.Fatal("SANDBOX_TEST_LIBRARY must point to the matching Sentry shared library")
	}
	cmd := exec.Command("go", "list", "-json", "reflect")
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Dir          string
		XTestGoFiles []string
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, name := range pkg.XTestGoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			if fn.Name.Name == "TestMain" {
				t.Fatal("upstream reflect now has a TestMain; review the sandbox entry")
			}
			names = append(names, fn.Name.Name)
		}
	}
	if len(names) == 0 {
		t.Fatal("no upstream reflect tests found")
	}
	sort.Strings(names)
	var entries strings.Builder
	for _, name := range names {
		fmt.Fprintf(&entries, "\t{%q, %s},\n", name, name)
	}
	dir, err := os.MkdirTemp("", "sandbox-reflect-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	mainPath, driverPath := filepath.Join(dir, "main.go"), filepath.Join(dir, "driver.go")
	if err := os.WriteFile(mainPath, []byte(strings.Replace(sandboxMain, "// TESTS", entries.String(), 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driverPath, []byte(sandboxDriver), 0600); err != nil {
		t.Fatal(err)
	}
	// Keep the upstream internal imports and export_test.go in their original
	// package context. Overlay files exist only for this test's compilation.
	overlay := struct{ Replace map[string]string }{map[string]string{
		filepath.Join(pkg.Dir, "sandbox_entry_test.go"):                                 mainPath,
		filepath.Join(runtime.GOROOT(), "src", "testing", "sandboxdriver", "driver.go"): driverPath,
	}}
	data, err = json.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "reflect.test")
	cmd = exec.Command("go", "test", "-mod=readonly", "-overlay="+overlayPath,
		"-ldflags=-checklinkname=0 -s=false -w=false", "-c", "-o", binary, "reflect")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build upstream reflect: %v\n%s", err, output)
	}
	t.Logf("%s: %d upstream reflect tests", runtime.Version(), len(names))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-sandbox-reflect="+name)
			cmd.Dir = pkg.Dir
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("reflect/Sentry: %v (context: %v)\n%s", err, ctx.Err(), output)
			}
			if bytes.Contains(output, []byte("--- SKIP: "+name+" ")) {
				t.Skipf("upstream skipped:\n%s", output)
			}
			if !bytes.Contains(output, []byte("--- PASS: "+name+" ")) {
				t.Fatalf("upstream test did not report completion:\n%s", output)
			}
			t.Logf("%s", output)
		})
	}
}
