package reflectxtype

import (
	"encoding/binary"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

import "github.com/goplus/reflectx"

func TestSharedMethodEntries(t *testing.T) {
	if os.Getenv("SANDBOX_SHARED_METHODS") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestSharedMethodEntries$", "-test.v")
		command.Env = append(os.Environ(), "SANDBOX_SHARED_METHODS=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("shared methods: %v\n%s", err, output)
		}
		return
	}
	const count = 10
	ctx := reflectx.NewContext()
	ctx.SetHasImethod(func(_ reflect.Type, m reflectx.Method) bool { return m.Name != "Reflected" })
	number := func(args []reflect.Value) []reflect.Value {
		return []reflect.Value{reflect.ValueOf(int(args[0].Field(0).Elem().Int()))}
	}
	add := func(args []reflect.Value) []reflect.Value {
		value := args[0].Elem().Field(0).Elem()
		for i := 0; i < args[1].Len(); i++ {
			value.SetInt(value.Int() + args[1].Index(i).Int())
		}
		return []reflect.Value{reflect.ValueOf(int(value.Int()))}
	}
	constant := func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.ValueOf(42)} }
	var originals []reflect.Type
	for range count {
		typ := ctx.NewMethodSet(reflectx.NamedTypeOf("example/shared", "T", reflect.TypeFor[struct{ N *int }]()), 1, 4)
		methods := []reflectx.Method{
			reflectx.MakeMethod("Number", "", false, reflect.TypeFor[func() int](), number),
			reflectx.MakeMethod("Add", "", true, reflect.TypeFor[func(...int) int](), add),
			reflectx.MakeMethod("Reflected", "", true, reflect.TypeFor[func() int](), constant),
			reflectx.MakeMethod("Local", "", true, reflect.TypeFor[func() int](), constant),
		}
		for i := 0; i < 3; i++ {
			methods[i].FuncId = i + 1
		}
		if err := ctx.SetMethodSet(typ, methods, false); err != nil {
			t.Fatal(err)
		}
		reflect.SliceOf(typ)
		originals = append(originals, typ)
	}
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snapshot.Methods); got != count+3 {
		t.Fatalf("callbacks: got %d, want %d", got, count+3)
	}
	table, err := Open(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	callbacks := make([]func([]reflect.Value) []reflect.Value, table.MethodCount())
	shared := make(map[string]int)
	locals := make(map[int]bool)
	for _, typ := range originals {
		for _, method := range table.definitions[snapshot.IDs[typ]-1].methods {
			switch method.name {
			case "Local":
				if locals[method.function] {
					t.Fatal("merged uncached methods with the same callback")
				}
				locals[method.function] = true
				callbacks[method.function-1] = constant
			case "Number", "Add", "Reflected":
				if previous := shared[method.name]; previous != 0 && previous != method.function {
					t.Fatalf("%s lost its shared callback ID", method.name)
				}
				shared[method.name] = method.function
				switch method.name {
				case "Number":
					callbacks[method.function-1] = number
				case "Add":
					callbacks[method.function-1] = add
				case "Reflected":
					callbacks[method.function-1] = constant
				}
			}
		}
	}
	_, before, _ := reflectx.IcallStat()
	cached := reflectx.IcallCached()
	if err := table.SetMethods(callbacks); err != nil {
		t.Fatal(err)
	}
	_, after, _ := reflectx.IcallStat()
	if after-before != count+3 || table.ctx.IcallAlloc() != count+3 || reflectx.IcallCached() != cached {
		t.Fatalf("slots: allocated=%d owned=%d cached=%d, want %d owned and no new global cache slots", after-before, table.ctx.IcallAlloc(), reflectx.IcallCached()-cached, count+3)
	}
	entries := make(map[string]methodEntries)
	for _, original := range originals {
		typ, err := table.Resolve(snapshot.IDs[original])
		if err != nil {
			t.Fatal(err)
		}
		value := reflect.New(typ)
		n := 10
		value.Elem().Field(0).Set(reflect.ValueOf(&n))
		if got := value.Interface().(interface{ Add(...int) int }).Add(2, 5); got != 17 {
			t.Fatalf("pointer variadic method: %d", got)
		}
		for _, receiver := range []reflect.Value{value, value.Elem()} {
			if got := receiver.Interface().(interface{ Number() int }).Number(); got != 17 {
				t.Fatalf("shared value method: %d", got)
			}
		}
		if got := value.Interface().(interface{ Local() int }).Local(); got != 42 {
			t.Fatalf("uncached method: %d", got)
		}
		method, _ := reflectx.MethodByName(value.Type(), "Reflected")
		if got := method.Func.Call([]reflect.Value{value})[0].Int(); got != 42 {
			t.Fatalf("reflection-only method: %d", got)
		}
		methods, _, _, current := concreteMethodSet(typ)
		for j, method := range methods {
			if method.Name == "Local" {
				continue
			}
			if previous, ok := entries[method.Name]; ok && previous != current[j] {
				t.Fatalf("%s did not reuse its guest entries", method.Name)
			}
			entries[method.Name] = current[j]
		}
	}
	returned, err := table.Export()
	if err != nil {
		t.Fatal(err)
	}
	host, err := snapshot.Open(returned.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range originals {
		if got, err := host.Resolve(snapshot.IDs[original]); err != nil || got != original {
			t.Fatalf("retained type changed: %v, %v", got, err)
		}
	}
	table.ctx.Reset()
	_, reset, _ := reflectx.IcallStat()
	if reset != before {
		t.Fatalf("reset left method slots: got %d, want %d", reset, before)
	}
}

func TestInvalidSharedMethodReferences(t *testing.T) {
	// Two named integer types each declare the pointer method F. Only their
	// callback IDs vary; changing the name also invalidates a shared reference.
	for _, test := range []struct {
		name string
		ids  [2]uint64
		last string
	}{
		{"missing", [2]uint64{2, 2}, "F"},
		{"out_of_range", [2]uint64{1, 3}, "F"},
		{"zero", [2]uint64{0, 1}, "F"},
		{"different_method", [2]uint64{1, 1}, "G"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := binary.AppendUvarint(nil, 3)
			for i, id := range test.ids {
				entry := binary.AppendUvarint(nil, dynamic|concreteMethods|uint64(reflect.Int))
				entry = appendString(entry, "T")
				entry = appendString(entry, "example/invalid")
				entry = binary.AppendUvarint(entry, uint64(reflect.TypeFor[int]().Size()))
				entry = binary.AppendUvarint(entry, uint64(reflect.TypeFor[int]().Align()))
				entry = appendFlag(entry, true)
				entry = binary.AppendUvarint(entry, 1)
				name := "F"
				if i == 1 {
					name = test.last
				}
				entry = appendString(entry, name)
				entry = appendString(entry, "")
				entry = appendFlag(entry, true)
				entry = appendFlag(entry, true)
				entry = binary.AppendUvarint(entry, 3)
				entry = binary.AppendUvarint(entry, id)
				data = binary.AppendUvarint(data, uint64(len(entry)))
				data = append(data, entry...)
			}
			entry := binary.AppendUvarint(nil, dynamic|uint64(reflect.Func))
			entry = appendString(entry, "")
			entry = appendString(entry, "")
			entry = binary.AppendUvarint(entry, uint64(reflect.TypeFor[func()]().Size()))
			entry = binary.AppendUvarint(entry, uint64(reflect.TypeFor[func()]().Align()))
			entry = append(entry, 0, 0, 0, 0)
			data = binary.AppendUvarint(data, uint64(len(entry)))
			data = append(data, entry...)
			if _, err := Open(data); err == nil {
				t.Fatal("accepted invalid callback references")
			}
		})
	}
}
