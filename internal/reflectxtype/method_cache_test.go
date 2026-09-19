package reflectxtype

import (
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/goplus/reflectx"
)

// Keep one closure PC so different_captures checks that equal code addresses
// do not merge Read methods whose captured increments are different.
//
//go:noinline
func methodCacheReader(increment int) func([]reflect.Value) []reflect.Value {
	return func(args []reflect.Value) []reflect.Value {
		n := args[0].Elem().Field(0).Int()
		return []reflect.Value{reflect.ValueOf(int(n) + increment)}
	}
}

func TestMethodCacheIdentity(t *testing.T) {
	for _, test := range []struct {
		name              string
		funcIDs           [2]int
		differentCaptures bool
		implementations   int
	}{
		// First.Read and Second.Read share an existing reflectx cache entry.
		{"shared_func_id", [2]int{1, 1}, false, 1},
		// The callback is identical, but these are separate reflectx entries.
		{"different_func_ids", [2]int{1, 2}, false, 2},
		{"without_func_id", [2]int{0, 0}, false, 2},
		// Both callbacks have the same PC and signature, but return N+1 / N+2.
		{"different_captures", [2]int{0, 0}, true, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			// FuncId and reflect type caches are global. Each scenario needs a
			// fresh process so another scenario cannot populate FuncId 1 first.
			if os.Getenv("SANDBOX_METHOD_CACHE_IDENTITY") != test.name {
				cmd := exec.Command(os.Args[0], "-test.run=^TestMethodCacheIdentity$/^"+test.name+"$", "-test.v")
				cmd.Env = append(os.Environ(), "SANDBOX_METHOD_CACHE_IDENTITY="+test.name)
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("method cache: %v\n%s", err, output)
				}
				t.Logf("%s", output)
				return
			}

			ctx := reflectx.NewContext()
			t.Cleanup(ctx.Reset)
			read := methodCacheReader(1)
			sourceCallbacks := [2]func([]reflect.Value) []reflect.Value{read, read}
			increments := [2]int{1, 1}
			if test.differentCaptures {
				sourceCallbacks[1] = methodCacheReader(2)
				increments[1] = 2
				if reflect.ValueOf(sourceCallbacks[0]).Pointer() != reflect.ValueOf(sourceCallbacks[1]).Pointer() {
					t.Fatal("fixture callbacks must have the same PC")
				}
			}
			var originals [2]reflect.Type
			for i, name := range []string{"First", "Second"} {
				typ := ctx.NewMethodSet(reflectx.NamedTypeOf("example/cache", name, reflect.TypeFor[struct{ N int }]()), 0, 1)
				method := reflectx.MakeMethod("Read", "", true, reflect.TypeFor[func() int](), sourceCallbacks[i])
				method.FuncId = test.funcIDs[i]
				if err := ctx.SetMethodSet(typ, []reflectx.Method{method}, false); err != nil {
					t.Fatal(err)
				}
				reflect.SliceOf(typ)
				originals[i] = typ
			}

			// Export keeps both types. Only an already-shared method gets one ID.
			sent, err := Export()
			if err != nil {
				t.Fatal(err)
			}
			if sent.IDs[originals[0]] == 0 || sent.IDs[originals[1]] == 0 || sent.IDs[originals[0]] == sent.IDs[originals[1]] {
				t.Fatal("method deduplication lost the distinct receiver types")
			}
			guest, err := Open(sent.Data)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(guest.ctx.Reset)
			if len(sent.Methods) != test.implementations || guest.MethodCount() != test.implementations {
				t.Fatalf("method implementations: exported=%d imported=%d, want %d", len(sent.Methods), guest.MethodCount(), test.implementations)
			}
			var ids [2]int
			callbacks := make([]func([]reflect.Value) []reflect.Value, guest.MethodCount())
			for i, typ := range originals {
				methods := guest.definitions[sent.IDs[typ]-1].methods
				if len(methods) != 1 || methods[0].name != "Read" {
					t.Fatalf("%v lost its Read declaration: %v", typ, methods)
				}
				ids[i] = methods[0].function
				callbacks[ids[i]-1] = sourceCallbacks[i]
				fn := sent.Methods[ids[i]-1]
				receiver := reflect.New(fn.Type().In(0).Elem())
				receiver.Elem().Field(0).SetInt(10)
				if got := fn.Call([]reflect.Value{receiver})[0].Int(); got != int64(10+increments[i]) {
					t.Fatalf("%v exported callback returned %d, want %d", typ, got, 10+increments[i])
				}
			}
			if (ids[0] == ids[1]) != (test.implementations == 1) {
				t.Fatalf("Read MethodIDs=%v, want %d distinct implementations", ids, test.implementations)
			}
			t.Logf("export: First.Read -> %d, Second.Read -> %d; %d implementations", ids[0], ids[1], len(sent.Methods))

			// Import allocates one icall per distinct pointer-method entry.
			_, before, _ := reflectx.IcallStat()
			cached := reflectx.IcallCached()
			if err := guest.SetMethods(callbacks); err != nil {
				t.Fatal(err)
			}
			_, after, _ := reflectx.IcallStat()
			if after-before != test.implementations || guest.ctx.IcallAlloc() != test.implementations {
				t.Fatalf("guest icall slots: allocated=%d owned=%d, want %d", after-before, guest.ctx.IcallAlloc(), test.implementations)
			}
			if reflectx.IcallCached() != cached {
				t.Fatal("import added entries to the process-global method cache")
			}
			var restored [2]reflect.Type
			var results [2]int
			for i, typ := range originals {
				restored[i], err = guest.Resolve(sent.IDs[typ])
				if err != nil {
					t.Fatal(err)
				}
				if restored[i] == typ {
					t.Fatal("fresh import reused the host type")
				}
				value := reflect.New(restored[i])
				value.Elem().Field(0).SetInt(int64(10 * (i + 1)))
				results[i] = value.Interface().(interface{ Read() int }).Read()
				want := 10*(i+1) + increments[i]
				if results[i] != want {
					t.Fatalf("%v.Read returned %d, want %d", restored[i], results[i], want)
				}
				method, ok := reflectx.MethodByName(value.Type(), "Read")
				if !ok || method.Func.Call([]reflect.Value{value})[0].Int() != int64(want) {
					t.Fatalf("%v.Read reflection call lost its receiver or capture", restored[i])
				}
			}
			if restored[0] == restored[1] {
				t.Fatal("shared methods merged two receiver types")
			}
			t.Logf("import: %d icall slots; First{10}.Read()=%d, Second{20}.Read()=%d", after-before, results[0], results[1])

			// Returning preserves IDs and reuses the original host type identities.
			returned, err := guest.Export()
			if err != nil {
				t.Fatal(err)
			}
			host, err := sent.Open(returned.Data)
			if err != nil {
				t.Fatal(err)
			}
			for i, typ := range originals {
				id := sent.IDs[typ]
				if returned.IDs[restored[i]] != id || host.definitions[id-1].methods[0].function != ids[i] {
					t.Fatalf("%v changed its TypeID or MethodID on return", typ)
				}
				if got, err := host.Resolve(id); err != nil || got != typ {
					t.Fatalf("%v lost its host identity: got %v, err=%v", typ, got, err)
				}
			}
			guest.ctx.Reset()
			_, remaining, _ := reflectx.IcallStat()
			if remaining != before {
				t.Fatalf("guest reset left %d icall slots", remaining-before)
			}
		})
	}
}
