//go:build linux && (amd64 || arm64) && cgo

// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in testdata/LICENSE.

package ixgo_test

import (
	"reflect"
	"testing"
)

// These cases adapt public API checks from Go 1.26.6 reflect/all_test.go,
// type_test.go and set_test.go. Objects are created before Run; assertions
// also check aliases and mutations after the guest returns.
type reflectCounter struct{ N int }

func (c *reflectCounter) Add(n int) int { c.N += n; return c.N }

type reflectAdder interface{ Add(int) int }
type reflectIntPointer *int
type reflectOtherIntPointer *int

func publicReflectTypes() []reflect.Type {
	number := reflect.TypeFor[int]()
	return []reflect.Type{
		number, reflect.TypeFor[any](), reflect.TypeFor[reflectAdder](),
		reflect.PointerTo(number), reflect.ArrayOf(3, number), reflect.SliceOf(number),
		reflect.MapOf(reflect.TypeFor[string](), number),
		reflect.ChanOf(reflect.BothDir, number),
		reflect.ChanOf(reflect.RecvDir, number),
		reflect.ChanOf(reflect.SendDir, number),
		reflect.StructOf([]reflect.StructField{{Name: "Count", Type: number, Tag: `json:"count"`}}),
		reflect.FuncOf([]reflect.Type{number}, []reflect.Type{number}, false),
		reflect.FuncOf([]reflect.Type{number, reflect.SliceOf(number)}, []reflect.Type{number}, true),
	}
}

func TestReflectPublicTypes(t *testing.T) {
	types := publicReflectTypes()
	var returned []reflect.Type
	if err := runSandbox(t, func() {
		local := publicReflectTypes()
		for i, typ := range types {
			if typ != local[i] {
				panic("restored type differs from the guest constructor")
			}
		}
		field := types[10].Field(0)
		if field.Name != "Count" || field.Tag.Get("json") != "count" || field.Type != types[0] {
			panic("StructOf field metadata changed")
		}
		if !types[12].IsVariadic() || types[12].In(1) != types[5] || types[12].Out(0) != types[0] {
			panic("FuncOf signature changed")
		}
		returned = local
	}); err != nil {
		t.Fatal(err)
	}
	for i, typ := range types {
		if i >= len(returned) || returned[i] != typ {
			t.Fatalf("returned type %d lost host identity", i)
		}
	}
}

func TestReflectPublicStructValue(t *testing.T) {
	typ := reflect.StructOf([]reflect.StructField{
		{Name: "Count", Type: reflect.TypeFor[int](), Tag: `json:"count"`},
		{Name: "Name", Type: reflect.TypeFor[string]()},
	})
	value := reflect.New(typ).Elem()
	value.Field(0).SetInt(41)
	value.Field(1).SetString("host")
	count := value.Field(0).Addr().Interface().(*int)
	if err := runSandbox(t, func() {
		if value.Type() != typ || !value.CanSet() || value.Field(0).Addr().Interface().(*int) != count {
			panic("reflected struct lost its type, access or field alias")
		}
		value.FieldByName("Count").SetInt(42)
		value.FieldByName("Name").SetString("guest")
		if *count != 42 {
			panic("reflected field mutation lost alias")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if *count != 42 || value.Field(0).Int() != 42 || value.Field(1).String() != "guest" || value.Type() != typ {
		t.Fatal("reflected struct mutation was not written back")
	}
}

func TestReflectPublicContainers(t *testing.T) {
	array := [3]int{1, 2, 3}
	slice := array[:]
	arrayValue, sliceValue := reflect.ValueOf(&array).Elem(), reflect.ValueOf(slice)
	mapping := reflect.MakeMap(reflect.TypeFor[map[string][]int]())
	mapping.SetMapIndex(reflect.ValueOf("shared"), sliceValue)
	if err := runSandbox(t, func() {
		arrayValue.Index(1).SetInt(20)
		fromMap := mapping.MapIndex(reflect.ValueOf("shared"))
		if sliceValue.Index(1).Int() != 20 || fromMap.Index(1).Int() != 20 || slice[1] != 20 {
			panic("array, slice and map lost their shared backing storage")
		}
		fromMap.Index(2).SetInt(30)
		mapping.SetMapIndex(reflect.ValueOf("grown"), reflect.Append(sliceValue, reflect.ValueOf(4)))
	}); err != nil {
		t.Fatal(err)
	}
	if array != [3]int{1, 20, 30} || &slice[0] != &array[0] {
		t.Fatal("array or slice identity was not written back")
	}
	shared := mapping.MapIndex(reflect.ValueOf("shared")).Interface().([]int)
	grown := mapping.MapIndex(reflect.ValueOf("grown")).Interface().([]int)
	if &shared[0] != &array[0] || !reflect.DeepEqual(grown, []int{1, 20, 30, 4}) {
		t.Fatal("reflected map or appended slice was not written back")
	}
}

func TestReflectPublicMethods(t *testing.T) {
	counter := &reflectCounter{N: 10}
	var boxed any = counter
	value := reflect.ValueOf(&boxed).Elem()
	method := reflect.ValueOf(counter).MethodByName("Add")
	var result int
	if err := runSandbox(t, func() {
		if value.Elem().Interface().(*reflectCounter) != counter {
			panic("interface lost receiver identity")
		}
		if method.Call([]reflect.Value{reflect.ValueOf(2)})[0].Int() != 12 {
			panic("bound reflected method")
		}
		m, ok := reflect.TypeOf(counter).MethodByName("Add")
		if !ok || m.Func.Call([]reflect.Value{reflect.ValueOf(counter), reflect.ValueOf(3)})[0].Int() != 15 {
			panic("reflected method expression")
		}
		result = value.Elem().Interface().(reflectAdder).Add(4)
	}); err != nil {
		t.Fatal(err)
	}
	if result != 19 || counter.N != 19 || boxed.(*reflectCounter) != counter {
		t.Fatal("method receiver mutation was not written back")
	}
	if got := method.Call([]reflect.Value{reflect.ValueOf(1)})[0].Int(); got != 20 || counter.N != 20 {
		t.Fatal("original reflected method lost its receiver")
	}
}

func TestReflectPublicMakeFunc(t *testing.T) {
	n := 10
	typ := reflect.FuncOf([]reflect.Type{reflect.TypeFor[int](), reflect.TypeFor[[]int]()}, []reflect.Type{reflect.TypeFor[int]()}, true)
	value := reflect.MakeFunc(typ, func(in []reflect.Value) []reflect.Value {
		n += int(in[0].Int())
		for i := 0; i < in[1].Len(); i++ {
			n += int(in[1].Index(i).Int())
		}
		return []reflect.Value{reflect.ValueOf(n)}
	})
	fn := value.Interface().(func(int, ...int) int)
	var results [3]int
	if err := runSandbox(t, func() {
		if value.Type() != typ {
			panic("MakeFunc signature changed")
		}
		results[0] = fn(1, 2, 3)
		results[1] = int(value.Call([]reflect.Value{reflect.ValueOf(4), reflect.ValueOf(5)})[0].Int())
		results[2] = int(value.CallSlice([]reflect.Value{reflect.ValueOf(6), reflect.ValueOf([]int{7, 8})})[0].Int())
	}); err != nil {
		t.Fatal(err)
	}
	if results != [3]int{16, 25, 46} || n != 46 {
		t.Fatalf("MakeFunc results=%v capture=%d", results, n)
	}
	if got := fn(1); got != 47 || n != 47 {
		t.Fatal("original MakeFunc lost its captured variable")
	}
}

func TestReflectPublicAssignableTo(t *testing.T) {
	cases := []struct {
		from, to reflect.Type
		want     bool
	}{
		{reflect.TypeFor[chan int](), reflect.TypeFor[<-chan int](), true},
		{reflect.TypeFor[<-chan int](), reflect.TypeFor[chan int](), false},
		{reflect.TypeFor[*int](), reflect.TypeFor[reflectIntPointer](), true},
		{reflect.TypeFor[reflectIntPointer](), reflect.TypeFor[*int](), true},
		{reflect.TypeFor[reflectIntPointer](), reflect.TypeFor[reflectOtherIntPointer](), false},
		{reflect.TypeFor[*reflectCounter](), reflect.TypeFor[reflectAdder](), true},
		{reflect.TypeFor[reflectCounter](), reflect.TypeFor[reflectAdder](), false},
	}
	var checked int
	if err := runSandbox(t, func() {
		for _, test := range cases {
			if test.from.AssignableTo(test.to) != test.want {
				panic("AssignableTo changed after transfer")
			}
			if test.to.Kind() == reflect.Interface && test.from.Implements(test.to) != test.want {
				panic("Implements changed after transfer")
			}
			checked++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if checked != len(cases) {
		t.Fatalf("checked %d assignability cases, want %d", checked, len(cases))
	}
}
