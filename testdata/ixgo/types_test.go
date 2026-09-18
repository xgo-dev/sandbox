//go:build linux && (amd64 || arm64) && cgo

package ixgo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goplus/ixgo"
	_ "github.com/goplus/ixgo/pkg/path/filepath"
	_ "github.com/goplus/ixgo/pkg/sync/atomic"
)

// Adapt the ixgo v1.1.6 type cases into closures whose state exists before Run.
// The fixture comments identify the upstream cases and the added writeback checks.
func TestIxgoTypeRoundTrips(t *testing.T) {
	for _, test := range []struct {
		file, name string
	}{
		{"generic.go", "TypeParamNamed"},
		{"generic.go", "NestedTypeParams"},
		{"generic.go", "TypeParamsRecursive"},
		{"generic.go", "AtomicPointer"},
		{"methods.go", "ReflectArray"},
		{"methods.go", "AliasInterface"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join("testdata", "types", test.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			interp, err := ixgo.NewContext(ixgo.SupportMultipleInterp).LoadInterp(path, data)
			if err != nil {
				t.Fatal(err)
			}
			defer interp.UnsafeRelease()
			if err := interp.RunInit(); err != nil {
				t.Fatal(err)
			}
			value, err := interp.RunFunc("Prepare" + test.name)
			if err != nil {
				t.Fatal(err)
			}
			fn := value.(func() int)
			if got := fn(); got != 1 {
				t.Fatalf("host before Run: got %d, want 1", got)
			}
			var first, second int
			if err := runSandbox(t, func() {
				first, second = fn(), fn()
			}); err != nil {
				t.Fatal(err)
			}
			if first != 2 || second != 3 {
				t.Fatalf("guest: got %d, %d, want 2, 3", first, second)
			}
			if got := fn(); got != 4 {
				t.Fatalf("original host closure after Run: got %d, want 4", got)
			}
		})
	}
}
