// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflectxtype

import (
	"reflect"
	"sync"
	"unsafe"
)

type staticLocation struct {
	module uint32
	offset uint64
}

type staticTypeIndex struct {
	byType     map[reflect.Type]staticLocation
	byLocation map[staticLocation]reflect.Type
}

var staticTypes = sync.OnceValue(indexStaticTypes)

//go:linkname reflectTypeLinks reflect.typelinks
func reflectTypeLinks() ([]unsafe.Pointer, [][]int32)

//go:linkname reflectToType reflect.toType
func reflectToType(unsafe.Pointer) reflect.Type

// Go 1.26.6 reflectOffs with the default amd64/arm64 runtime mutex layout.
// Access the maps only while holding the runtime's lock; our transferMu does
// not serialize reflect constructors in other goroutines.
//
//go:linkname reflectOffsets runtime.reflectOffs
var reflectOffsets struct {
	lock uintptr
	next int32
	m    map[int32]unsafe.Pointer
	minv map[unsafe.Pointer]int32
}

//go:linkname reflectOffsetsLock runtime.reflectOffsLock
func reflectOffsetsLock()

//go:linkname reflectOffsetsUnlock runtime.reflectOffsUnlock
func reflectOffsetsUnlock()

func indexStaticTypes() *staticTypeIndex {
	sections, offsets := reflectTypeLinks()
	index := &staticTypeIndex{
		byType:     make(map[reflect.Type]staticLocation),
		byLocation: make(map[staticLocation]reflect.Type),
	}
	var queue []reflect.Type
	for module, section := range sections {
		for _, offset := range offsets[module] {
			queue = append(queue, reflectToType(unsafe.Add(section, offset)))
		}
	}
	// Typelinks includes *Node but not necessarily Node. Follow immutable
	// descriptor dependencies to include its fields and interface signatures.
	// Input offsets are resolved through this index, never dereferenced directly.
	for i := 0; i < len(queue); i++ {
		typ := queue[i]
		if _, ok := index.byType[typ]; ok {
			continue
		}
		address := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&typ))[1])
		location := staticLocation{offset: ^uint64(0)}
		for j, section := range sections {
			if base := uintptr(section); base <= address && uint64(address-base) < location.offset {
				location = staticLocation{uint32(j), uint64(address - base)}
			}
		}
		index.byType[typ] = location
		index.byLocation[location] = typ
		queue = appendDependencies(queue, typ)
	}
	return index
}

func appendDependencies(types []reflect.Type, typ reflect.Type) []reflect.Type {
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		types = append(types, typ.Elem())
	case reflect.Map:
		types = append(types, typ.Key(), typ.Elem())
	case reflect.Func:
		for i := 0; i < typ.NumIn(); i++ {
			types = append(types, typ.In(i))
		}
		for i := 0; i < typ.NumOut(); i++ {
			types = append(types, typ.Out(i))
		}
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			types = append(types, typ.Field(i).Type)
		}
	case reflect.Interface:
		for i := 0; i < typ.NumMethod(); i++ {
			types = append(types, typ.Method(i).Type)
		}
	}
	return types
}

var builtinTypes = [...]reflect.Type{
	reflect.Bool: reflect.TypeFor[bool](),
	reflect.Int:  reflect.TypeFor[int](), reflect.Int8: reflect.TypeFor[int8](),
	reflect.Int16: reflect.TypeFor[int16](), reflect.Int32: reflect.TypeFor[int32](), reflect.Int64: reflect.TypeFor[int64](),
	reflect.Uint: reflect.TypeFor[uint](), reflect.Uint8: reflect.TypeFor[uint8](),
	reflect.Uint16: reflect.TypeFor[uint16](), reflect.Uint32: reflect.TypeFor[uint32](), reflect.Uint64: reflect.TypeFor[uint64](),
	reflect.Uintptr: reflect.TypeFor[uintptr](),
	reflect.Float32: reflect.TypeFor[float32](), reflect.Float64: reflect.TypeFor[float64](),
	reflect.Complex64: reflect.TypeFor[complex64](), reflect.Complex128: reflect.TypeFor[complex128](),
	reflect.String: reflect.TypeFor[string](), reflect.UnsafePointer: reflect.TypeFor[unsafe.Pointer](),
}

// These layouts match Go 1.26.6. The two bucket caches are append-only; readers
// use sync.Map.Range without taking the writers' mutexes.
type cacheLayout struct {
	mu sync.Mutex
	m  sync.Map
}

//go:linkname pointerCache reflect.ptrMap
var pointerCache sync.Map

//go:linkname compositeCache reflect.lookupCache
var compositeCache sync.Map

//go:linkname functionCache reflect.funcLookupCache
var functionCache cacheLayout

//go:linkname structCache reflect.structLookupCache
var structCache cacheLayout

func cachedTypes() []reflect.Type {
	seen := make(map[reflect.Type]bool)
	var result []reflect.Type
	add := func(typ reflect.Type) {
		if !seen[typ] {
			seen[typ] = true
			result = append(result, typ)
		}
	}
	pointerCache.Range(func(_, value any) bool {
		add(reflectToType(reflect.ValueOf(value).UnsafePointer()))
		return true
	})
	compositeCache.Range(func(_, value any) bool {
		add(value.(reflect.Type))
		return true
	})
	functionCache.m.Range(func(_, value any) bool {
		bucket := reflect.ValueOf(value)
		for i := 0; i < bucket.Len(); i++ {
			add(reflectToType(bucket.Index(i).UnsafePointer()))
		}
		return true
	})
	structCache.m.Range(func(_, value any) bool {
		for _, typ := range value.([]reflect.Type) {
			add(typ)
		}
		return true
	})
	return result
}
