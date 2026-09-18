// Copyright 2022 The GoPlus Authors (goplus.org). All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Adapted from ixgo v1.1.6 typeparam_test.go: TestTypeParamNamed,
// TestNestedTypeParams, TestTypeParamsRecursive and TestAtomicPointer.
// Prepare the captured values on the host and check them on every invocation.
package main

import (
	"path/filepath"
	"reflect"
	"sync/atomic"
)

type N int
type _Nil[A any, B any] struct{}

func PrepareTypeParamNamed() func() int {
	var v1 _Nil[int, N]
	var v2 _Nil[filepath.WalkFunc, _Nil[int, N]]
	values := []any{v1, v2}
	types := []reflect.Type{reflect.TypeOf(v1), reflect.TypeOf(v2)}
	calls := 0
	return func() int {
		if s := types[0].String(); s != "main._Nil[int,main.N]" {
			panic(s)
		}
		if s := types[1].String(); s != "main._Nil[path/filepath.WalkFunc,main._Nil[int,main.N]]" {
			panic(s)
		}
		for i, value := range values {
			if reflect.TypeOf(value) != types[i] {
				panic("generic value and reflect.Type lost identity")
			}
		}
		calls++
		return calls
	}
}

func nested[N ~int](initial N) func() int {
	type T []N
	values := T{initial}
	typ := reflect.TypeOf(values)
	return func() int {
		if reflect.TypeOf(values) != typ {
			panic("local generic type changed")
		}
		values[0]++
		return int(values[0] - initial)
	}
}

func PrepareNestedTypeParams() func() int { return nested(N(100)) }

type Integer interface {
	~int | ~int32 | ~int64
}

func recur1[T Integer](n T) T {
	if n == 0 || n == 1 {
		return T(1)
	}
	return n * recur2(n-1)
}

func recur2[T Integer](n T) T {
	list := make([]T, n)
	for i := range list {
		list[i] = T(i + 1)
	}
	var sum T
	for _, value := range list {
		sum += value
	}
	return sum + recur1(n-1)
}

func PrepareTypeParamsRecursive() func() int {
	type T int
	n := T(5)
	calls := 0
	return func() int {
		if recur1(n) != T(110) {
			panic("recursive generic call")
		}
		calls++
		return calls
	}
}

func PrepareAtomicPointer() func() int {
	var i atomic.Int64
	i.Store(200)
	n, text := 200, "hello"
	var number atomic.Pointer[int]
	var word atomic.Pointer[string]
	number.Store(&n)
	word.Store(&text)
	calls := 0
	return func() int {
		if number.Load() != &n || word.Load() != &text || *word.Load() != "hello" {
			panic("atomic pointer lost target or alias")
		}
		calls++
		*number.Load()++
		if i.Add(1) != int64(200+calls) || n != 200+calls {
			panic("atomic value was not written back")
		}
		return calls
	}
}
