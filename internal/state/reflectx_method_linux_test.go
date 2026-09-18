//go:build linux && (amd64 || arm64)

package state

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/goplus/reflectx"
)

type methodNumber interface{ Number() int }
type methodAdder interface{ Add(...int) int }

type methodRoot struct {
	Type     reflect.Type
	Object   any
	Number   methodNumber
	Adder    methodAdder
	Counter  *int
	Function reflect.Value
}

func methodFixture() methodRoot {
	typ := reflectx.NamedTypeOf("example/methodgraph", "Counter", reflect.TypeFor[struct{ N int }]())
	typ = reflectx.NewMethodSet(typ, 1, 3)
	counter := new(int)
	*counter = 3
	number := func(args []reflect.Value) []reflect.Value {
		if args[0].Type() != typ {
			panic("method receiver type changed")
		}
		return []reflect.Value{reflect.ValueOf(int(args[0].Field(0).Int()) + *counter)}
	}
	add := func(args []reflect.Value) []reflect.Value {
		value := args[0].Elem().Field(0)
		for i := 0; i < args[1].Len(); i++ {
			value.SetInt(value.Int() + args[1].Index(i).Int())
		}
		*counter++
		return []reflect.Value{reflect.ValueOf(int(value.Int()))}
	}
	hidden := func([]reflect.Value) []reflect.Value { return nil }
	if err := reflectx.SetMethodSet(typ, []reflectx.Method{
		reflectx.MakeMethod("Number", "", false, reflect.TypeFor[func() int](), number),
		reflectx.MakeMethod("Add", "", true, reflect.TypeFor[func(...int) int](), add),
		reflectx.MakeMethod("hidden", "example/private", true, reflect.TypeFor[func()](), hidden),
	}, false); err != nil {
		panic(err)
	}
	value := reflect.New(typ)
	value.Elem().Field(0).SetInt(10)
	method, _ := reflectx.MethodByName(typ, "Number")
	return methodRoot{Type: typ, Object: value.Interface(), Number: value.Interface().(methodNumber), Adder: value.Interface().(methodAdder), Counter: counter, Function: method.Func}
}

func checkMethodRoot(t *testing.T, got methodRoot) {
	t.Helper()
	value := reflect.ValueOf(got.Object)
	if value.Type().Elem() != got.Type || got.Number != got.Object || got.Adder != got.Object {
		t.Fatal("method receiver identity changed")
	}
	runtime.GC()
	if got.Number.Number() != 13 {
		t.Fatal("interface lost value receiver or capture")
	}
	if got.Function.Call([]reflect.Value{value.Elem()})[0].Int() != 13 {
		t.Fatal("function and method disagree")
	}
	if got.Adder.Add(2, 5) != 17 || *got.Counter != 4 || got.Number.Number() != 21 {
		t.Fatal("variadic method lost mutation or captured alias")
	}
	if value.Elem().Interface().(methodNumber).Number() != 21 {
		t.Fatal("value receiver interface failed")
	}
	private := reflectx.InterfaceOf(nil, []reflect.Method{{Name: "hidden", PkgPath: "example/private", Type: reflect.TypeFor[func()]()}})
	other := reflectx.NewContext().InterfaceOf(nil, []reflect.Method{{Name: "hidden", PkgPath: "example/other", Type: reflect.TypeFor[func()]()}})
	if !value.Type().Implements(private) || value.Type().Implements(other) {
		t.Fatal("private method package identity changed")
	}
	method, ok := reflectx.MethodByName(value.Type(), "hidden")
	if !ok {
		t.Fatal("private method missing")
	}
	method.Func.Call([]reflect.Value{value})
}

func TestReflectxMethodGraph(t *testing.T) {
	if os.Getenv("SANDBOX_METHOD_GRAPH_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxMethodGraph$", "-test.v")
		cmd.Env = append(os.Environ(), "SANDBOX_METHOD_GRAPH_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("method graph: %v\n%s", err, output)
		}
		return
	}
	src := methodFixture()
	var dst methodRoot
	roundtrip(t, &src, &dst)
	checkMethodRoot(t, dst)
	if *src.Counter != 3 || src.Number.Number() != 13 {
		t.Fatal("guest changed source captures")
	}
	var next methodRoot
	roundtrip(t, &dst, &next)
	if next.Number.Number() != 21 || next.Adder.Add(1) != 18 || *next.Counter != 5 || *dst.Counter != 4 {
		t.Fatal("fresh state lost the restored method's captures or source isolation")
	}
}

func TestReflectxMethodNewProcess(t *testing.T) {
	const imageEnv = "SANDBOX_METHOD_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got methodRoot
		if _, err := Load(context.Background(), data, &got); err != nil {
			t.Fatal(err)
		}
		checkMethodRoot(t, got)
		return
	}
	if os.Getenv("SANDBOX_METHOD_IMAGE_SOURCE") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxMethodNewProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "SANDBOX_METHOD_IMAGE_SOURCE=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("method source: %v\n%s", err, output)
		}
		return
	}
	src := methodFixture()
	mem := make([]byte, 8<<20)
	n, _, err := Save(context.Background(), mem, &src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "methods.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxMethodNewProcess$", "-test.v")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("method destination: %v\n%s", err, output)
	}
}

type NativeMethodCounter struct{ N int }

func (n *NativeMethodCounter) Add(values ...int) int {
	for _, value := range values {
		n.N += value
	}
	return n.N
}

func TestReflectxNativeMethodProcess(t *testing.T) {
	const imageEnv = "SANDBOX_NATIVE_METHOD_IMAGE"
	if os.Getenv("SANDBOX_NATIVE_METHOD_SOURCE") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxNativeMethodProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "SANDBOX_NATIVE_METHOD_SOURCE=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("native method source: %v\n%s", err, output)
		}
		return
	}

	embed := func(receiver any, tag reflect.StructTag) reflect.Value {
		value := reflect.ValueOf(receiver)
		typ := reflect.StructOf([]reflect.StructField{{Name: value.Type().Elem().Name(), Type: value.Type(), Anonymous: true, Tag: tag}})
		result := reflect.New(typ).Elem()
		result.Field(0).Set(value)
		return result
	}
	ctx := context.Background()
	var graph State
	mem := make([]byte, 8<<20)
	if path := os.Getenv(imageEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var guest []reflect.Value
		if _, err := graph.Load(ctx, data, &guest); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		checkMethodRoot(t, guest[3].Interface().(methodRoot))
		if got := guest[0].Interface().(interface{ String() string }).String(); got != "host" {
			t.Fatalf("native String method = %q", got)
		}
		if got := guest[1].Interface().(methodAdder).Add(2, 5); got != 17 {
			t.Fatalf("native variadic method = %d", got)
		}
		guest[0].MethodByName("WriteString").Call([]reflect.Value{reflect.ValueOf("-guest")})
		guest[2] = embed(bytes.NewBufferString("created in guest"), `origin:"guest"`)
		n, _, err := graph.Save(ctx, mem, &guest)
		if err != nil {
			t.Fatal(err)
		}
		checkOriginalMethodRecords(t, graph.saved, mem[:n])
		if err := os.WriteFile(path, mem[:n], 0600); err != nil {
			t.Fatal(err)
		}
		return
	}

	buffer := bytes.NewBufferString("host")
	counter := &NativeMethodCounter{N: 10}
	// Mix native promoted methods with reflectx callbacks, as an interpreter
	// does when its dynamic types include an embedded bytes.Buffer.
	host := []reflect.Value{embed(buffer, ""), embed(counter, ""), {}, reflect.ValueOf(methodFixture())}
	n, _, err := graph.Save(ctx, mem, &host)
	if err != nil {
		t.Fatal(err)
	}
	checkOriginalMethodRecords(t, graph.saved, mem[:n])
	path := filepath.Join(t.TempDir(), "native-methods.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReflectxNativeMethodProcess$", "-test.v")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native method destination: %v\n%s", err, output)
	}
	if buffer.String() != "host" || counter.N != 10 {
		t.Fatal("guest changed source receivers before writeback")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Load(ctx, data, &host); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if buffer.String() != "host-guest" || counter.N != 17 {
		t.Fatal("returned methods lost their receiver aliases")
	}
	if got := host[1].Interface().(methodAdder).Add(3, 4); got != 24 || counter.N != 24 {
		t.Fatalf("returned variadic method = %d, receiver = %d", got, counter.N)
	}
	if got := host[2].Interface().(interface{ String() string }).String(); got != "created in guest" {
		t.Fatalf("guest-created native String method = %q", got)
	}
}

func checkOriginalMethodRecords(t *testing.T, es *encodeState, data []byte) {
	t.Helper()
	r := reader{mem: data}
	for range 2 {
		n, objects, err := readHeader(&r)
		if err != nil || objects {
			t.Fatalf("type table header: objects=%t err=%v", objects, err)
		}
		r.readBytes(n)
	}
	encoded, err := r.get()
	if err != nil {
		t.Fatal(err)
	}
	methods, ok := encoded.(*arrayValue)
	if !ok {
		t.Fatalf("method table is %T", encoded)
	}
	var native, dynamic int
	for _, record := range methods.Contents {
		value, ok := record.(*reflectedValue)
		if !ok || value.Addressable {
			t.Fatalf("method is not an original function value: %T", record)
		}
		fn, ok := value.Value.(*functionValue)
		if !ok {
			t.Fatalf("method payload is %T", value.Value)
		}
		if uintptr(fn.PC) == makeFuncPC {
			dynamic++
		} else {
			native++
			if fn.Env.Root != 0 {
				t.Fatalf("native method acquired a wrapper environment: %v", fn.Env)
			}
		}
	}
	if native == 0 || dynamic == 0 {
		t.Fatalf("missing method kind: native=%d dynamic=%d", native, dynamic)
	}
	callPC := reflect.ValueOf(reflect.Value{}.Call).Pointer()
	callSlicePC := reflect.ValueOf(reflect.Value{}.CallSlice).Pointer()
	for _, obj := range es.pending {
		pc := es.native.storage[obj.obj.Type()]
		if pc == reflectxMethodCallPC || pc == callPC || pc == callSlicePC {
			t.Fatalf("local method adapter entered the object graph: ID %d", obj.id)
		}
	}
}
