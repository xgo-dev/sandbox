// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflectxtype

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/goplus/reflectx"
)

func sampleTypes() []reflect.Type {
	ctx := reflectx.NewContext()
	integer := reflect.TypeFor[int]()
	embedded := reflectx.NamedTypeOf("example/model", "Embedded", integer)
	node := reflectx.NamedTypeOf("example/model", "Node", ctx.StructOf([]reflect.StructField{
		{Name: "N", Type: integer}, {Name: "Next", Type: reflect.TypeFor[*struct{}]()},
	}))
	reflectx.SetUnderlying(node, ctx.StructOf([]reflect.StructField{
		{Name: "N", Type: integer}, {Name: "Next", Type: reflectx.PtrTo(node)},
	}))
	fn := reflectx.NamedTypeOf("example/model", "F", reflect.TypeFor[func(int) int]())
	reflectx.SetUnderlying(fn, reflect.FuncOf([]reflect.Type{fn}, []reflect.Type{fn}, false))
	variadic := reflectx.NamedTypeOf("example/model", "V", reflect.TypeFor[func(...int) int]())
	reflectx.SetUnderlying(variadic, reflect.FuncOf([]reflect.Type{reflect.SliceOf(variadic)}, []reflect.Type{variadic}, true))
	namedSlice := reflectx.NamedTypeOf("example/model", "Args", reflect.SliceOf(node))
	variadicSlice := reflect.FuncOf([]reflect.Type{namedSlice}, nil, true)
	iface := ctx.InterfaceOf(nil, []reflect.Method{
		{Name: "Get", Type: reflect.FuncOf(nil, []reflect.Type{node}, false)},
		{Name: "hidden", PkgPath: "example/private", Type: reflect.TypeFor[func()]()},
	})
	otherIface := reflectx.NewContext().InterfaceOf(nil, []reflect.Method{
		{Name: "Get", Type: reflect.FuncOf(nil, []reflect.Type{node}, false)},
		{Name: "hidden", PkgPath: "example/other", Type: reflect.TypeFor[func()]()},
	})
	namedIface := reflectx.NamedTypeOf("example/model", "I", iface)
	recursiveIface := reflectx.NewInterfaceType("example/model", "RecursiveI")
	if err := reflectx.SetInterfaceType(recursiveIface, nil, []reflect.Method{
		{Name: "Next", Type: reflect.FuncOf(nil, []reflect.Type{recursiveIface}, false)},
	}); err != nil {
		panic(err)
	}
	strct := ctx.StructOf([]reflect.StructField{
		{Name: "Embedded", Type: embedded, Anonymous: true},
		{Name: "Value", Type: node, Tag: `json:"value"`},
		{Name: "private", PkgPath: "example/model", Type: reflect.TypeFor[string]()},
		{Name: "_", PkgPath: "example/model", Type: integer},
		{Name: "_", PkgPath: "example/model", Type: integer},
	})
	types := []reflect.Type{node, embedded, fn, variadic, iface, otherIface, namedIface, recursiveIface, strct, namedSlice, variadicSlice}
	for _, value := range []any{false, int(0), int8(0), int16(0), int32(0), int64(0), uint(0), uint8(0), uint16(0), uint32(0), uint64(0), uintptr(0), float32(0), float64(0), complex64(0), complex128(0), "", unsafe.Pointer(nil)} {
		typ := reflect.TypeOf(value)
		types = append(types, reflectx.NamedTypeOf("example/model", "Named"+strings.ReplaceAll(typ.Kind().String(), ".", ""), typ))
	}
	for i, typ := range []reflect.Type{
		reflectx.PtrTo(node), reflect.SliceOf(node), reflect.ArrayOf(3, node),
		reflect.MapOf(node, reflect.TypeFor[error]()),
		reflect.ChanOf(reflect.BothDir, node), reflect.ChanOf(reflect.RecvDir, node), reflect.ChanOf(reflect.SendDir, node),
		strct,
	} {
		types = append(types, typ, reflectx.NamedTypeOf("example/model", fmt.Sprintf("Container%d", i), typ))
	}
	// The same spelling does not imply the same runtime type identity.
	types = append(types, reflectx.NamedTypeOf("example/model", "Node", node))
	for _, typ := range types {
		reflect.SliceOf(typ)
	}
	return types
}

func assertType(t *testing.T, want, got reflect.Type, seen map[reflect.Type]reflect.Type) {
	t.Helper()
	if previous, ok := seen[want]; ok {
		if previous != got {
			t.Fatalf("lost identity for %v", want)
		}
		return
	}
	seen[want] = got
	if want.String() != got.String() {
		t.Fatalf("type spelling: %v => %v", want, got)
	}
	if want.Kind() != got.Kind() || want.Name() != got.Name() || want.PkgPath() != got.PkgPath() || want.Size() != got.Size() || want.Align() != got.Align() || want.Comparable() != got.Comparable() {
		t.Fatalf("type mismatch: %v (%v) => %v (%v)", want, want.Kind(), got, got.Kind())
	}
	switch want.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		assertType(t, want.Elem(), got.Elem(), seen)
		if want.Kind() == reflect.Array && want.Len() != got.Len() {
			t.Fatal("array length")
		}
		if want.Kind() == reflect.Chan && want.ChanDir() != got.ChanDir() {
			t.Fatal("channel direction")
		}
	case reflect.Map:
		assertType(t, want.Key(), got.Key(), seen)
		assertType(t, want.Elem(), got.Elem(), seen)
	case reflect.Func:
		if want.NumIn() != got.NumIn() || want.NumOut() != got.NumOut() || want.IsVariadic() != got.IsVariadic() {
			t.Fatal("function signature")
		}
		for i := 0; i < want.NumIn(); i++ {
			assertType(t, want.In(i), got.In(i), seen)
		}
		for i := 0; i < want.NumOut(); i++ {
			assertType(t, want.Out(i), got.Out(i), seen)
		}
	case reflect.Struct:
		if want.NumField() != got.NumField() {
			t.Fatal("field count")
		}
		for i := 0; i < want.NumField(); i++ {
			w, g := want.Field(i), got.Field(i)
			if w.Name != g.Name || w.PkgPath != g.PkgPath || w.Tag != g.Tag || w.Offset != g.Offset || w.Anonymous != g.Anonymous {
				t.Fatalf("field mismatch: %+v => %+v", w, g)
			}
			assertType(t, w.Type, g.Type, seen)
		}
	case reflect.Interface:
		if want.NumMethod() != got.NumMethod() {
			t.Fatal("method count")
		}
		for i := 0; i < want.NumMethod(); i++ {
			w, g := want.Method(i), got.Method(i)
			if w.Name != g.Name || w.PkgPath != g.PkgPath {
				t.Fatalf("method identity: %+v => %+v", w, g)
			}
			assertType(t, w.Type, g.Type, seen)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	want := sampleTypes()
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[reflect.Type]reflect.Type)
	for _, typ := range want {
		id := snapshot.IDs[typ]
		if id == 0 {
			t.Fatalf("missing cached type %v", typ)
		}
		value, err := got.Resolve(id)
		if err != nil {
			t.Fatal(err)
		}
		assertType(t, typ, value, seen)
	}
	if seen[want[0]] == seen[want[len(want)-1]] {
		t.Fatal("merged distinct named types")
	}
	clear(snapshot.Data)
	checkNode(t, seen[want[0]])
	if _, err := got.Resolve(0); err == nil {
		t.Fatal("accepted ID 0")
	}
	if _, err := got.Resolve(uint32(len(got.types)) + 1); err == nil {
		t.Fatal("accepted unknown ID")
	}
	second, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	if second.IDs[seen[want[0]]] == 0 {
		t.Fatal("restored type cannot be re-exported")
	}
	if _, err := Open(second.Data); err != nil {
		t.Fatal(err)
	}
}

func checkNode(t *testing.T, node reflect.Type) {
	t.Helper()
	if node.Field(1).Type.Elem() != node {
		t.Fatal("recursive identity")
	}
	first, second := reflect.New(node), reflect.New(node)
	first.Elem().Field(0).SetInt(17)
	second.Elem().Field(0).SetInt(42)
	first.Elem().Field(1).Set(second)
	second = reflect.Value{}
	runtime.GC()
	if first.Elem().Field(1).Elem().Field(0).Int() != 42 {
		t.Fatal("GC lost referenced value")
	}
	values := reflect.MakeMap(reflect.MapOf(node, reflect.TypeFor[string]()))
	values.SetMapIndex(first.Elem(), reflect.ValueOf("found"))
	copy := reflect.New(node).Elem()
	copy.Set(first.Elem())
	if values.MapIndex(copy).String() != "found" {
		t.Fatal("map key equality")
	}
}

func TestFreshProcess(t *testing.T) {
	if os.Getenv("SANDBOX_REFLECTXTYPE_CHILD") == "1" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		types, err := Open(data[4:])
		if err != nil {
			t.Fatal(err)
		}
		node, err := types.Resolve(binary.LittleEndian.Uint32(data[:4]))
		if err != nil {
			t.Fatal(err)
		}
		checkNode(t, node)
		return
	}
	node := sampleTypes()[0]
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	data := binary.LittleEndian.AppendUint32(nil, snapshot.IDs[node])
	data = append(data, snapshot.Data...)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFreshProcess$")
	cmd.Env = append(os.Environ(), "SANDBOX_REFLECTXTYPE_CHILD=1")
	cmd.Stdin = bytes.NewReader(data)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest: %v\n%s", err, output)
	}
}

func TestDefaultCache(t *testing.T) {
	typ := reflectx.InterfaceOf(nil, []reflect.Method{{Name: "DefaultCache", Type: reflect.TypeFor[func()]()}})
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.IDs[typ] == 0 {
		t.Fatal("missing Default cache root")
	}
}

func TestRecursiveStorage(t *testing.T) {
	ctx := reflectx.NewContext()
	left := reflectx.NamedTypeOf("example/cycle", "Left", reflect.TypeFor[struct{ Link *int }]())
	right := reflectx.NamedTypeOf("example/cycle", "Right", reflect.TypeFor[[2]*int]())
	reflectx.SetUnderlying(left, ctx.StructOf([]reflect.StructField{{Name: "Link", Type: reflectx.PtrTo(right)}}))
	reflectx.SetUnderlying(right, reflect.ArrayOf(2, reflectx.PtrTo(left)))
	list := reflectx.NamedTypeOf("example/cycle", "List", reflect.TypeFor[[]int]())
	reflectx.SetUnderlying(list, reflect.SliceOf(list))
	index := reflectx.NamedTypeOf("example/cycle", "Index", reflect.TypeFor[map[string]int]())
	reflectx.SetUnderlying(index, reflect.MapOf(reflect.TypeFor[string](), index))
	// Blank fields must be ignored by equality, even in arrays constructed
	// before this named descriptor has its final underlying definition.
	blank := reflectx.NamedTypeOf("example/cycle", "Blank", ctx.StructOf([]reflect.StructField{
		{Name: "_", PkgPath: "example/cycle", Type: reflect.TypeFor[int]()},
		{Name: "_", PkgPath: "example/cycle", Type: reflect.TypeFor[int]()},
		{Name: "N", Type: reflect.TypeFor[int]()},
	}))
	array := reflect.ArrayOf(1, blank)
	want := []reflect.Type{left, right, list, index, array, blank}
	for _, typ := range want {
		reflect.SliceOf(typ)
	}
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	table, err := Open(snapshot.Data)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[reflect.Type]reflect.Type)
	for _, typ := range want {
		got, err := table.Resolve(snapshot.IDs[typ])
		if err != nil {
			t.Fatal(err)
		}
		assertType(t, typ, got, seen)
	}
	first, second := reflect.New(seen[array]).Elem(), reflect.New(seen[array]).Elem()
	*(*int)(unsafe.Pointer(first.Index(0).Field(0).UnsafeAddr())) = 42
	*(*int)(unsafe.Pointer(second.Index(0).Field(1).UnsafeAddr())) = 99
	if first.Interface() != second.Interface() {
		t.Fatal("array equality compares blank fields")
	}
	values := reflect.MakeMap(reflect.MapOf(seen[array], reflect.TypeFor[int]()))
	values.SetMapIndex(first, reflect.ValueOf(7))
	if result := values.MapIndex(second); !result.IsValid() || result.Int() != 7 {
		t.Fatal("blank-field map key")
	}
	value := reflect.New(seen[left])
	link := reflect.New(seen[right])
	link.Elem().Index(1).Set(value)
	value.Elem().Field(0).Set(link)
	runtime.GC()
	if value.Elem().Field(0).Elem().Index(1).Pointer() != value.Pointer() {
		t.Fatal("mutually recursive values")
	}
}

func TestConcurrentOpen(t *testing.T) {
	typ := sampleTypes()[0]
	snapshot, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			table, err := Open(snapshot.Data)
			if err != nil {
				t.Error(err)
				return
			}
			node, err := table.Resolve(snapshot.IDs[typ])
			if err != nil {
				t.Error(err)
				return
			}
			checkNode(t, node)
		})
	}
	group.Wait()
}

func TestConcreteMethods(t *testing.T) {
	// Keep method-slot allocations and fixture caches in a separate process.
	if os.Getenv("SANDBOX_REFLECTXTYPE_METHOD_CHILD") == "1" {
		for _, pointer := range []bool{false, true} {
			base := reflectx.NamedTypeOf("example/methods", "T", reflect.TypeFor[int]())
			typ := reflectx.NewMethodSet(base, 1, 1)
			callback := func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.ValueOf(42)} }
			method := reflectx.MakeMethod("hidden", "example/methods", pointer, reflect.TypeFor[func() int](), callback)
			if err := reflectx.SetMethodSet(typ, []reflectx.Method{method}, false); err != nil {
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
			if err := table.SetMethods(callbacks); err != nil {
				t.Fatal(err)
			}
			got, err := table.Resolve(snapshot.IDs[typ])
			if err != nil {
				t.Fatal(err)
			}
			methods, functions, _, _ := concreteMethodSet(got)
			if len(methods) != 1 || methods[0].Name != "hidden" || methods[0].PkgPath != "example/methods" || methods[0].Pointer != pointer {
				t.Fatalf("method metadata: %+v", methods)
			}
			receiver := reflect.New(got)
			if !pointer {
				receiver = receiver.Elem()
			}
			if functions[0].Call([]reflect.Value{receiver})[0].Int() != 42 {
				t.Fatal("restored method returned wrong value")
			}
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestConcreteMethods$")
	cmd.Env = append(os.Environ(), "SANDBOX_REFLECTXTYPE_METHOD_CHILD=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("method fixture: %v\n%s", err, output)
	}
}

func TestMethodFunctionIndices(t *testing.T) {
	for _, tc := range []struct {
		name    string
		indices []uint64
		valid   bool
	}{
		{"record end", []uint64{1}, true},
		{"out of order", []uint64{2, 1}, true},
		{"zero", []uint64{0}, false},
		{"out of range", []uint64{2}, false},
		{"duplicate", []uint64{1, 1}, false},
		{"overflow", []uint64{^uint64(0)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := binary.AppendUvarint(nil, dynamic|concreteMethods|uint64(reflect.Int))
			owner = appendString(owner, "T")
			owner = appendString(owner, "example/methodindices")
			owner = binary.AppendUvarint(owner, uint64(reflect.TypeFor[int]().Size()))
			owner = binary.AppendUvarint(owner, uint64(reflect.TypeFor[int]().Align()))
			owner = appendFlag(owner, true)
			owner = binary.AppendUvarint(owner, uint64(len(tc.indices)))
			for i, index := range tc.indices {
				owner = appendString(owner, fmt.Sprintf("M%d", i))
				owner = appendString(owner, "")
				owner = appendFlag(owner, false)
				owner = appendFlag(owner, true)
				owner = binary.AppendUvarint(owner, 2)
				owner = binary.AppendUvarint(owner, index)
			}
			signature := binary.AppendUvarint(nil, dynamic|uint64(reflect.Func))
			signature = appendString(signature, "")
			signature = appendString(signature, "")
			signature = binary.AppendUvarint(signature, uint64(reflect.TypeFor[func()]().Size()))
			signature = binary.AppendUvarint(signature, uint64(reflect.TypeFor[func()]().Align()))
			signature = append(signature, 0, 0, 0, 0) // Comparable, variadic, inputs, outputs.
			data := binary.AppendUvarint(nil, 2)
			for _, entry := range [][]byte{owner, signature} {
				data = binary.AppendUvarint(data, uint64(len(entry)))
				data = append(data, entry...)
			}
			table, err := Open(data)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				if table.MethodCount() != len(tc.indices) {
					t.Fatal("incorrect callback count")
				}
			} else if err == nil || table != nil || !strings.Contains(err.Error(), "invalid method function index") {
				t.Fatalf("Open = %v, %v", table, err)
			}
		})
	}
}

func TestInvalidData(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty": nil, "truncated": {1}, "trailing": {0, 1}, "empty entry": {1, 0},
		"integer overflow": bytes.Repeat([]byte{255}, 11), "unknown kind": {1, 1, 255},
		"missing static": {1, 3, 0, 127, 127}, "trailing entry": {1, 2, byte(reflect.Int), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if table, err := Open(data); err == nil || table != nil {
				t.Fatalf("Open = %v, %v", table, err)
			}
		})
	}
	entry := func(kind reflect.Kind, name string, size uint64, payload ...uint64) []byte {
		data := binary.AppendUvarint(nil, dynamic|uint64(kind))
		data = appendString(data, name)
		data = appendString(data, "")
		data = binary.AppendUvarint(data, size)
		data = binary.AppendUvarint(data, 8)
		data = appendFlag(data, kind != reflect.Slice && kind != reflect.Map && kind != reflect.Func)
		for _, n := range payload {
			data = binary.AppendUvarint(data, n)
		}
		return data
	}
	for name, entries := range map[string][][]byte{
		"zero ID":                {entry(reflect.Pointer, "", 8, 0)},
		"unknown ID":             {entry(reflect.Pointer, "", 8, 2)},
		"inline cycle":           {entry(reflect.Array, "A", 8, 1, 1)},
		"unnamed cycle":          {entry(reflect.Slice, "", 24, 1)},
		"wrong size":             {entry(reflect.Int, "Int", 1)},
		"wrong field offset":     {append(entry(reflect.Struct, "", 8, 1), 1, 'N', 0, 0, 0, 7, 2), {byte(reflect.Int)}},
		"bad channel direction":  {entry(reflect.Chan, "", 8, 2, 9), {byte(reflect.Int)}},
		"bad method kind":        {append(entry(reflect.Interface, "", 16, 1), 1, 'M', 0, 2), {byte(reflect.Int)}},
		"variadic without input": {entry(reflect.Func, "F", 8, 1, 0, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			data := binary.AppendUvarint(nil, uint64(len(entries)))
			for _, item := range entries {
				data = binary.AppendUvarint(data, uint64(len(item)))
				data = append(data, item...)
			}
			if table, err := Open(data); err == nil || table != nil {
				t.Fatalf("Open = %v, %v", table, err)
			}
		})
	}
}
