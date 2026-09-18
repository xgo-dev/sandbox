// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in ../LICENSE.

// Adapted from ixgo v1.1.6 interp_test.go: TestReflectArray and
// TestRuntimeRefelct. Keep receivers, reflected values and interfaces alive
// across the sandbox boundary and check mutations through their aliases.
package main

import (
	"fmt"
	"reflect"
)

type IntArray [2]int

func (a IntArray) String() string       { return fmt.Sprintf("(%v,%v)", a[0], a[1]) }
func (a *IntArray) Set(x, y int)        { a[0], a[1] = x, y }
func (a IntArray) Get() (int, int)      { return a[0], a[1] }
func (a IntArray) Scale(n int) IntArray { return IntArray{a[0] * n, a[1] * n} }

func PrepareReflectArray() func() int {
	a := &IntArray{100, 200}
	typ := reflect.TypeOf(a).Elem()
	v := reflect.ValueOf(a).Elem()
	calls := 0
	return func() int {
		if v.Type() != typ || v.Addr().Interface().(*IntArray) != a {
			panic("reflected array lost type or receiver")
		}
		calls++
		v.Addr().MethodByName("Set").Call([]reflect.Value{
			reflect.ValueOf(100 + calls), reflect.ValueOf(200 + calls),
		})
		b := a.Scale(5)
		result := v.MethodByName("Scale").Call([]reflect.Value{reflect.ValueOf(5)})[0]
		if b[0] != 5*(100+calls) || b[1] != 5*(200+calls) ||
			result.Index(0).Int() != int64(b[0]) || result.Index(1).Int() != int64(b[1]) {
			panic("direct and reflected array methods disagree")
		}
		return calls
	}
}

type Game struct{ Runs int }

func (g *Game) MainEntry() { g.Runs++ }

type Alias = Game
type Entry interface{ MainEntry() }

func PrepareAliasInterface() func() int {
	g := &Alias{}
	var entry Entry = g
	var boxed any = entry
	typ := reflect.TypeOf(g)
	return func() int {
		if reflect.TypeOf(boxed) != typ || typ != reflect.TypeOf((*Game)(nil)) || boxed.(*Game) != g {
			panic("alias, interface or receiver identity changed")
		}
		entry.MainEntry()
		return g.Runs
	}
}
