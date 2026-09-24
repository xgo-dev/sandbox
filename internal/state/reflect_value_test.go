package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestReflectValues(t *testing.T) {
	type count int
	values := []reflect.Value{
		{}, reflect.ValueOf(false), reflect.ValueOf(42), reflect.ValueOf(count(7)),
		reflect.ValueOf(uint64(99)), reflect.ValueOf(float64(1.5)), reflect.ValueOf(complex(2, 3)),
		reflect.ValueOf("text"), reflect.ValueOf([2]int{1, 2}),
		reflect.ValueOf([]int{3, 4}), reflect.ValueOf([]int(nil)), reflect.ValueOf([]int{}),
		reflect.ValueOf(map[string]int{"n": 5}), reflect.ValueOf(map[string]int(nil)),
		reflect.ValueOf((*int)(nil)), reflect.ValueOf(reflectedNamed{Value: 8}),
		reflect.ValueOf(reflect.TypeFor[int]()),
		reflect.Zero(reflect.TypeFor[any]()),
		reflect.Zero(reflect.TypeFor[reflect.Type]()),
		reflect.Zero(reflect.TypeFor[func()]()),
		reflect.ValueOf(struct{ V any }{42}).Field(0),
		reflect.ValueOf(struct{ V any }{(*int)(nil)}).Field(0),
		reflect.ValueOf(struct{ V reflectedInterface }{reflectedNamed{Value: 9}}).Field(0),
	}
	for _, addressable := range []bool{false, true} {
		src := append([]reflect.Value(nil), values...)
		if addressable {
			for i, value := range src {
				if value.IsValid() {
					src[i] = reflect.New(value.Type()).Elem()
					src[i].Set(value)
				}
			}
		}
		var dst []reflect.Value
		roundtrip(t, &src, &dst)
		runtime.GC()
		for i, value := range src {
			got := dst[i]
			if !value.IsValid() {
				if got.IsValid() {
					t.Fatalf("value %d: invalid value became valid", i)
				}
				continue
			}
			if !got.IsValid() || got.Type() != value.Type() || got.CanAddr() != value.CanAddr() || got.CanSet() != value.CanSet() || !got.CanInterface() {
				t.Fatalf("value %d: type or access changed: source=%v decoded=%v", i, value, got)
			}
			if !reflect.DeepEqual(got.Interface(), value.Interface()) {
				t.Fatalf("value %d: got %#v, want %#v", i, got.Interface(), value.Interface())
			}
		}
	}
}

func TestReflectValueAliases(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		n := &graphNode{Value: 42}
		n.Next = n
		views := []reflect.Value{
			reflect.ValueOf(&n.Value).Elem(), reflect.ValueOf(n).Elem(), reflect.ValueOf(n),
		}
		src := []any{views, n}
		if parentFirst {
			src[0], src[1] = src[1], src[0]
		}
		var dst []any
		roundtrip(t, &src, &dst)
		if parentFirst {
			dst[0], dst[1] = dst[1], dst[0]
		}
		got, parent := dst[0].([]reflect.Value), dst[1].(*graphNode)
		if parent.Next != parent || !parent.loaded || got[0].Addr().Interface().(*int64) != &parent.Value || got[1].Addr().Interface().(*graphNode) != parent || got[2].Interface().(*graphNode) != parent {
			t.Fatal("reflected field, parent, cycle or callback lost")
		}
		got[0].SetInt(99)
		if parent.Value != 99 || n.Value != 42 {
			t.Fatal("reflected write lost aliases or changed the source")
		}
	}
}

func TestReflectValueVariables(t *testing.T) {
	type root struct {
		Pointer *int
		Slice   []int
		Any     any
		Views   []reflect.Value
	}
	n := 42
	src := root{Pointer: &n, Slice: []int{1, 2}, Any: &n}
	src.Views = []reflect.Value{
		reflect.ValueOf(&src.Pointer).Elem(),
		reflect.ValueOf(&src.Slice).Elem(),
		reflect.ValueOf(&src.Any).Elem(),
	}
	var dst root
	roundtrip(t, &src, &dst)
	if dst.Pointer != dst.Any.(*int) || dst.Views[0].Interface().(*int) != dst.Pointer {
		t.Fatal("pointer stored in a reflected variable changed")
	}
	dst.Views[0].SetZero()
	dst.Views[1].Set(reflect.ValueOf([]int{3, 4, 5}))
	dst.Views[2].Set(reflect.ValueOf("guest"))
	if dst.Pointer != nil || len(dst.Slice) != 3 || dst.Any != "guest" {
		t.Fatal("reflected variables are not backed by the restored fields")
	}
	if src.Pointer != &n || len(src.Slice) != 2 || src.Any != &n {
		t.Fatal("reflected writes modified source variables")
	}
}

func TestReflectValueSelfReference(t *testing.T) {
	var src reflect.Value
	src = reflect.ValueOf(&src).Elem()
	var dst reflect.Value
	roundtrip(t, &src, &dst)
	if dst.Type() != reflect.TypeFor[reflect.Value]() || dst.Addr().Interface().(*reflect.Value) != &dst {
		t.Fatal("reflect.Value no longer refers to its own variable")
	}
}

func TestReflectValueNested(t *testing.T) {
	n := 42
	inner := reflect.ValueOf(&n).Elem()
	src := map[string]reflect.Value{
		"nested": reflect.ValueOf(inner), "invalid": {}, "n": inner,
	}
	var dst map[string]reflect.Value
	roundtrip(t, &src, &dst)
	value := dst["nested"].Interface().(reflect.Value)
	value.SetInt(43)
	if dst["n"].Int() != 43 || n != 42 || dst["invalid"].IsValid() {
		t.Fatal("nested reflected value lost aliases or invalid state")
	}
}

func TestReflectValueAfterLoad(t *testing.T) {
	src := reflect.ValueOf(graphNode{Value: 42})
	var dst reflect.Value
	roundtrip(t, &src, &dst)
	got := dst.Interface().(graphNode)
	if got.Value != 42 || !got.loaded || dst.CanAddr() {
		t.Fatal("non-addressable reflected value lost its load callback")
	}
}

type reflectedWaiter struct {
	Value  reflect.Value
	Loaded bool
}

func (*reflectedWaiter) StateTypeName() string { return "state.test.reflectedWaiter" }
func (*reflectedWaiter) StateFields() []string { return []string{"Value"} }
func (v *reflectedWaiter) StateSave(s Sink)    { s.Save(0, &v.Value) }
func (v *reflectedWaiter) StateLoad(_ context.Context, s Source) {
	s.LoadValue(0, &v.Value, func(any) { v.Loaded = v.Value.Interface().(*graphNode).Value == 42 })
}

func init() { Register((*reflectedWaiter)(nil)) }

func TestReflectValueLoadWait(t *testing.T) {
	src := reflectedWaiter{Value: reflect.ValueOf(&graphNode{Value: 42})}
	var dst reflectedWaiter
	roundtrip(t, &src, &dst)
	if !dst.Loaded {
		t.Fatal("LoadValue did not wait for the reflected object's fields")
	}
}

func TestReflectValuePrivateFields(t *testing.T) {
	n := 42
	owner := struct {
		number   int
		text     string
		pointer  *int
		slice    []int
		array    [2]int
		mapping  map[string]int
		iface    any
		nilIface any
	}{42, "private", &n, []int{1, 2}, [2]int{3, 4}, map[string]int{"n": 5}, 6, nil}
	for _, parent := range []reflect.Value{reflect.ValueOf(owner), reflect.ValueOf(&owner).Elem()} {
		var src []reflect.Value
		for i := 0; i < parent.NumField(); i++ {
			src = append(src, parent.Field(i))
		}
		var dst []reflect.Value
		roundtrip(t, &src, &dst)
		runtime.GC()
		for i, value := range src {
			got := dst[i]
			if value.CanInterface() || got.CanInterface() || value.CanSet() || got.CanSet() || got.CanAddr() != value.CanAddr() || got.Type() != value.Type() {
				t.Fatalf("field %d: private access or type changed", i)
			}
			for _, call := range []func(){func() { got.Interface() }, func() { got.Set(reflect.Zero(got.Type())) }} {
				func() {
					defer func() {
						if recover() == nil {
							t.Errorf("field %d: restricted operation succeeded", i)
						}
					}()
					call()
				}()
			}
		}
		if dst[0].Int() != 42 || dst[1].String() != "private" || dst[2].Elem().Int() != 42 || dst[3].Index(1).Int() != 2 || dst[4].Index(1).Int() != 4 || dst[5].MapIndex(reflect.ValueOf("n")).Int() != 5 || dst[6].Elem().Int() != 6 || !dst[7].IsNil() {
			t.Fatal("private field data changed")
		}
		if dst[2].Pointer() == reflect.ValueOf(&n).Pointer() {
			t.Fatal("private pointer reused the source allocation")
		}
	}
}

type reflectedPrivateEmbedded struct {
	Exported int
	hidden   int
}

func TestReflectValuePrivateEmbedded(t *testing.T) {
	owner := struct {
		reflectedPrivateEmbedded
		named  reflectedPrivateEmbedded
		nested struct{ reflectedPrivateEmbedded }
	}{
		reflectedPrivateEmbedded: reflectedPrivateEmbedded{1, 2},
		named:                    reflectedPrivateEmbedded{3, 4},
		nested:                   struct{ reflectedPrivateEmbedded }{reflectedPrivateEmbedded{5, 6}},
	}
	for _, parent := range []reflect.Value{reflect.ValueOf(owner), reflect.ValueOf(&owner).Elem()} {
		// EmbedRO alone permits access to Exported; StickyRO, including the
		// combination from nested's private embedded field, must propagate.
		src := []reflect.Value{parent.Field(0), parent.Field(1), parent.Field(2).Field(0)}
		var dst []reflect.Value
		roundtrip(t, &src, &dst)
		for i, value := range src {
			got := dst[i]
			if value.CanInterface() || got.CanInterface() || got.CanSet() || got.CanAddr() != value.CanAddr() {
				t.Fatalf("embedded value %d changed access", i)
			}
			for j := 0; j < value.NumField(); j++ {
				before, after := value.Field(j), got.Field(j)
				if before.CanInterface() != after.CanInterface() || before.CanSet() != after.CanSet() || before.Int() != after.Int() {
					t.Fatalf("embedded value %d field %d lost access propagation", i, j)
				}
				if after.CanSet() {
					after.SetInt(99)
					if before.Int() == 99 {
						t.Fatal("restored embedded field still aliases the source")
					}
				}
			}
		}
	}
}

func TestReflectValuePrivateRoundTrip(t *testing.T) {
	type owner struct{ hidden int }
	type root struct {
		Owner    *owner
		Private  reflect.Value
		Writable reflect.Value
	}
	original := &owner{42}
	host := root{original, reflect.ValueOf(original).Elem().Field(0), reflect.ValueOf(&original.hidden).Elem()}
	var guest root
	var hostState, guestState State
	ctx := context.Background()
	mem := make([]byte, 1<<20)
	n, _, err := hostState.Save(ctx, mem, &host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guestState.Load(ctx, mem[:n], &guest); err != nil {
		t.Fatal(err)
	}
	if guest.Owner == original || guest.Private.UnsafeAddr() != guest.Writable.UnsafeAddr() {
		t.Fatal("private field aliases were not relocated")
	}
	guest.Writable.SetInt(43)
	if guest.Owner.hidden != 43 || guest.Private.Int() != 43 || original.hidden != 42 || guest.Private.CanInterface() || guest.Private.CanSet() {
		t.Fatal("private view lost its value, restrictions or source isolation")
	}
	n, _, err = guestState.Save(ctx, mem, &guest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hostState.Load(ctx, mem[:n], &host); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if host.Owner != original || original.hidden != 43 || host.Private.Int() != 43 || host.Private.UnsafeAddr() != reflect.ValueOf(&original.hidden).Pointer() || host.Private.CanInterface() || host.Private.CanSet() || !host.Writable.CanSet() {
		t.Fatal("private field writeback lost identity or access restrictions")
	}
}

func TestReflectValueNewProcess(t *testing.T) {
	type root struct {
		Value   reflect.Value
		Field   *int
		Type    reflect.Type
		Private reflect.Value
		Hidden  *int
	}
	const imageEnv = "SANDBOX_STATE_VALUE_TEST_IMAGE"
	if path := os.Getenv(imageEnv); path != "" {
		mem, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var dst root
		if _, err := Load(context.Background(), mem, &dst); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		if dst.Value.Type() != dst.Type || dst.Value.Field(0).Addr().Interface().(*int) != dst.Field || *dst.Field != 42 {
			t.Fatal("reflected type, value or alias changed in the child")
		}
		if dst.Private.Int() != 47 || dst.Private.CanInterface() || dst.Private.CanSet() || dst.Private.UnsafeAddr() != reflect.ValueOf(dst.Hidden).Pointer() {
			t.Fatal("private field access or alias changed in the child")
		}
		dst.Value.Field(0).SetInt(43)
		*dst.Hidden = 48
		output := make([]byte, 1<<20)
		n, _, err := Save(context.Background(), output, &dst)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".out", output[:n], 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	typ := reflect.StructOf([]reflect.StructField{{Name: "Count", Type: reflect.TypeFor[int](), Tag: `state:"value"`}})
	value := reflect.New(typ).Elem()
	value.Field(0).SetInt(42)
	hidden := struct{ hidden int }{47}
	src := root{
		Value: value, Field: value.Field(0).Addr().Interface().(*int), Type: typ,
		Private: reflect.ValueOf(&hidden).Elem().Field(0), Hidden: &hidden.hidden,
	}
	mem := make([]byte, 1<<20)
	n, _, err := Save(context.Background(), mem, &src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "value.state")
	if err := os.WriteFile(path, mem[:n], 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReflectValueNewProcess$")
	cmd.Env = append(os.Environ(), imageEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	output, err := os.ReadFile(path + ".out")
	if err != nil {
		t.Fatal(err)
	}
	var dst root
	if _, err := Load(context.Background(), output, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Value.Field(0).Int() != 43 || dst.Value.Field(0).Addr().Interface().(*int) != dst.Field || *src.Field != 42 {
		t.Fatal("return transfer lost value, aliases or source isolation")
	}
	if dst.Private.Int() != 48 || dst.Private.CanInterface() || dst.Private.CanSet() || dst.Private.UnsafeAddr() != reflect.ValueOf(dst.Hidden).Pointer() || hidden.hidden != 47 {
		t.Fatal("return transfer lost private field data, aliases or access restrictions")
	}
}
