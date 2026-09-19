package reflectxtype

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/goplus/reflectx"
)

func TestRetainedTypeIDs(t *testing.T) {
	original := sampleTypes()
	sent, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	guest, err := Open(sent.Data)
	if err != nil {
		t.Fatal(err)
	}
	node, err := guest.Resolve(sent.IDs[original[0]])
	if err != nil {
		t.Fatal(err)
	}
	added := reflectx.NamedTypeOf("example/return", "Added", reflect.SliceOf(node))
	reflect.SliceOf(added)
	returned, err := guest.Export()
	if err != nil {
		t.Fatal(err)
	}
	for typ, id := range sent.IDs {
		local, err := guest.Resolve(id)
		if err != nil || returned.IDs[local] != id {
			t.Fatalf("retained ID for %v: got %d, want %d, err=%v", typ, returned.IDs[local], id, err)
		}
	}
	if returned.IDs[added] <= uint32(len(sent.IDs)) {
		t.Fatal("new type reused a retained ID")
	}
	host, err := sent.Open(returned.Data)
	if err != nil {
		t.Fatal(err)
	}
	for typ, id := range sent.IDs {
		got, err := host.Resolve(id)
		if err != nil || got != typ {
			t.Fatalf("host did not reuse %v: got %v, err=%v", typ, got, err)
		}
	}
	got, err := host.Resolve(returned.IDs[added])
	if err != nil || got == added || got.Elem() != original[0] {
		t.Fatalf("new type did not resolve its retained dependency: %v, %v", got, err)
	}
	again, err := host.Export()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := returned.Open(again.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := restored.Resolve(returned.IDs[added]); err != nil || got != added {
		t.Fatalf("second return lost the new type's identity: %v, %v", got, err)
	}
}

func TestRetainedTypeDefinitionChanged(t *testing.T) {
	typ := reflectx.NamedTypeOf("example/retained", "OriginalName", reflect.TypeFor[int]())
	reflect.SliceOf(typ)
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.ReplaceAll(snapshot.Data, []byte("OriginalName"), []byte("DifferentOne"))
	if bytes.Equal(changed, snapshot.Data) {
		t.Fatal("fixture has no encoded name")
	}
	if _, err := snapshot.Open(changed); err == nil || !strings.Contains(err.Error(), "changed definition") {
		t.Fatalf("changed retained definition: %v", err)
	}
	if _, err := snapshot.Open(binary.AppendUvarint(nil, 0)); err == nil || !strings.Contains(err.Error(), "lost retained IDs") {
		t.Fatalf("missing retained definitions: %v", err)
	}
}

func TestRetainedMethodIDs(t *testing.T) {
	if os.Getenv("SANDBOX_REFLECTX_METHOD_ORDER") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestRetainedMethodIDs$", "-test.v")
		command.Env = append(os.Environ(), "SANDBOX_REFLECTX_METHOD_ORDER=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("method order: %v\n%s", err, output)
		}
		return
	}
	ctx := reflectx.NewContext()
	ctx.SetHasImethod(func(reflect.Type, reflectx.Method) bool { return false })
	typ := ctx.NewMethodSet(reflectx.NamedTypeOf("example/order", "T", reflect.TypeFor[int]()), 0, 4)
	type identity struct{ name, pkg string }
	order := []identity{
		{"execWith", "example/a"},
		{"initApp", "example/a"},
		{"execWith", "example/b"},
		{"initApp", "example/b"},
	}
	methods := make([]reflectx.Method, len(order))
	for i, key := range order {
		methods[i] = reflectx.MakeMethod(key.name, key.pkg, true, reflect.TypeFor[func() int](), func([]reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(i + 22)}
		})
	}
	// ixgo preserves go/types order; SetMethods reconstructs this table in
	// name order, moving example/b.execWith ahead of example/a.initApp.
	if err := ctx.SetRawMethods(typ, methods); err != nil {
		t.Fatal(err)
	}
	reflect.SliceOf(typ)
	sent, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	guest, err := Open(sent.Data)
	if err != nil {
		t.Fatal(err)
	}
	callbacks := make([]func([]reflect.Value) []reflect.Value, guest.MethodCount())
	def := guest.definitions[sent.IDs[typ]-1]
	if len(def.methods) != len(order) {
		t.Fatalf("got %d methods, want %d", len(def.methods), len(order))
	}
	for i, method := range def.methods {
		if got := (identity{method.name, method.pkg}); got != order[i] {
			t.Fatalf("source method %d: got %v, want %v", i, got, order[i])
		}
		callbacks[method.function-1] = func([]reflect.Value) []reflect.Value {
			return []reflect.Value{reflect.ValueOf(i + 22)}
		}
	}
	if err := guest.SetMethods(callbacks); err != nil {
		t.Fatal(err)
	}
	restored, err := guest.Resolve(sent.IDs[typ])
	if err != nil {
		t.Fatal(err)
	}
	current, _, _, _ := concreteMethodSet(restored)
	if current[1].Name != "execWith" || current[1].PkgPath != "example/b" {
		t.Fatalf("fixture did not reorder methods: %v", current)
	}
	returned, err := guest.Export()
	if err != nil {
		t.Fatal(err)
	}
	receiver := reflect.New(restored)
	for i, method := range def.methods {
		got := returned.Methods[method.function-1].Call([]reflect.Value{receiver})[0].Int()
		if got != int64(i+22) {
			t.Fatalf("method %s.%s ID %d returned %d, want %d", method.pkg, method.name, method.function, got, i+22)
		}
	}
	host, err := sent.Open(returned.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := host.Resolve(sent.IDs[typ]); err != nil || got != typ {
		t.Fatalf("host type changed: got %v, want %v, err=%v", got, typ, err)
	}
}

func TestMethodInterfacePolicy(t *testing.T) {
	if os.Getenv("SANDBOX_REFLECTX_METHOD_POLICY") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestMethodInterfacePolicy$", "-test.v")
		command.Env = append(os.Environ(), "SANDBOX_REFLECTX_METHOD_POLICY=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("method policy: %v\n%s", err, output)
		}
		return
	}
	ctx := reflectx.NewContext()
	ctx.SetHasImethod(func(_ reflect.Type, method reflectx.Method) bool {
		return method.Name == "Enabled"
	})
	typ := ctx.NewMethodSet(reflectx.NamedTypeOf("example/policy", "T", reflect.TypeFor[int]()), 0, 2)
	callback := func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.ValueOf(42)} }
	if err := ctx.SetMethodSet(typ, []reflectx.Method{
		reflectx.MakeMethod("Enabled", "", true, reflect.TypeFor[func() int](), callback),
		reflectx.MakeMethod("Reflected", "", true, reflect.TypeFor[func() int](), callback),
	}, false); err != nil {
		t.Fatal(err)
	}
	reflect.SliceOf(typ)
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	table, err := Open(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	callbacks := make([]func([]reflect.Value) []reflect.Value, table.MethodCount())
	for i := range callbacks {
		callbacks[i] = callback
	}
	_, before, _ := reflectx.IcallStat()
	if err := table.SetMethods(callbacks); err != nil {
		t.Fatal(err)
	}
	_, after, _ := reflectx.IcallStat()
	if after-before != 1 {
		t.Fatalf("interface policy allocated %d slots, want 1", after-before)
	}
	restored, err := table.Resolve(snapshot.IDs[typ])
	if err != nil {
		t.Fatal(err)
	}
	value := reflect.New(restored)
	if got := value.Interface().(interface{ Enabled() int }).Enabled(); got != 42 {
		t.Fatalf("interface method returned %d, want 42", got)
	}
	method, ok := reflectx.MethodByName(value.Type(), "Reflected")
	if !ok {
		t.Fatal("reflection method is missing")
	}
	if got := method.Func.Call([]reflect.Value{value})[0].Int(); got != 42 {
		t.Fatalf("reflection method returned %d, want 42", got)
	}
	if _, err := table.Export(); err != nil {
		t.Fatalf("interface policy changed during return: %v", err)
	}
}
