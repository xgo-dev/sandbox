// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflectxtype

import (
	"fmt"
	"go/token"
	"reflect"
	"unsafe"

	"github.com/goplus/reflectx"
)

// These descriptors match reflectx v1.7.8. A FuncId cache hit copies both
// descriptors verbatim; their identity survives without the process-local ID.
type runtimeMethod struct {
	name, signature, ifn, tfn int32
}

type methodEntries struct {
	value, pointer runtimeMethod
}

type methodIdentity struct {
	name, pkg string
	pointer   bool
}

//go:linkname runtimeMethods github.com/goplus/reflectx.rtypeMethods
func runtimeMethods(unsafe.Pointer) []runtimeMethod

//go:linkname resolveMethodName reflect.resolveNameOff
func resolveMethodName(unsafe.Pointer, int32) unsafe.Pointer

//go:linkname methodPackage reflect.pkgPath
func methodPackage(struct{ bytes *byte }) string

//go:linkname methodText reflect.resolveTextOff
func methodText(unsafe.Pointer, int32) unsafe.Pointer

//go:linkname zeroMethod github.com/goplus/reflectx.zeroIfn
var zeroMethod unsafe.Pointer

func concreteMethodSet(typ reflect.Type) ([]reflectx.Method, []reflect.Value, []bool, []methodEntries) {
	var methods []reflectx.Method
	var functions []reflect.Value
	var entries []methodEntries
	type identity struct{ name, pkg string }
	values := make(map[identity]int)
	interfaces := make(map[identity]bool)
	for _, receiver := range []reflect.Type{typ, reflectx.PtrTo(typ)} {
		pointer := receiver != typ
		rt := (*[2]unsafe.Pointer)(unsafe.Pointer(&receiver))[1]
		raw := runtimeMethods(rt)
		for i := 0; i < reflectx.NumMethodX(receiver); i++ {
			signature := raw[i].signature
			if signature == 0 || signature == -1 {
				continue
			}
			if signature < 0 {
				// StructOf can register nil when promoting a stripped method,
				// e.g. bytes.Buffer.tryGrowByReslice. MethodX would then fatal
				// inside resolveTypeOff, before it can return a nil Type.
				reflectOffsetsLock()
				pointer, found := reflectOffsets.m[signature]
				reflectOffsetsUnlock()
				if found && pointer == nil {
					continue
				}
			}
			method := reflectx.MethodX(receiver, i)
			if method.Type == nil {
				continue
			}
			pkg := method.PkgPath
			if !token.IsExported(method.Name) {
				name := resolveMethodName(rt, raw[i].name)
				pkg = methodPackage(struct{ bytes *byte }{(*byte)(name)})
			}
			key := identity{method.Name, pkg}
			if pointer {
				interfaces[key] = methodText(rt, raw[i].ifn) != zeroMethod
			}
			if pointer {
				if index, ok := values[key]; ok {
					entries[index].pointer = raw[i]
					continue
				}
			}
			values[key] = len(methods)
			entry := methodEntries{value: raw[i]}
			if pointer {
				entry = methodEntries{pointer: raw[i]}
			}
			entries = append(entries, entry)
			in, out := make([]reflect.Type, method.Type.NumIn()-1), make([]reflect.Type, method.Type.NumOut())
			for j := range in {
				in[j] = method.Type.In(j + 1)
			}
			for j := range out {
				out[j] = method.Type.Out(j)
			}
			methods = append(methods, reflectx.Method{Name: method.Name, PkgPath: pkg, Pointer: pointer, Type: reflect.FuncOf(in, out, method.Type.IsVariadic())})
			functions = append(functions, method.Func)
		}
	}
	hasInterface := make([]bool, len(methods))
	for i, method := range methods {
		hasInterface[i] = interfaces[identity{method.Name, method.PkgPath}]
	}
	return methods, functions, hasInterface, entries
}

// MethodCount is the number of unique callbacks required by SetMethods, in the
// same order as Snapshot.Methods. Types are not callable until installation.
func (t *ReflectType) MethodCount() int { return t.methodCount }

// SetMethods installs callbacks whose environments may still be undergoing
// graph restoration. No callback may run until the caller finishes that graph.
func (t *ReflectType) SetMethods(callbacks []func([]reflect.Value) []reflect.Value) (err error) {
	if len(callbacks) != t.methodCount {
		return fmt.Errorf("reflectx method callbacks: got %d, want %d", len(callbacks), t.methodCount)
	}
	transferMu.Lock()
	defer transferMu.Unlock()
	defer t.ctx.SetHasImethod(nil)
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("install reflectx methods: %v", failure)
		}
	}()
	installed := make(map[int]methodEntries)
	for i, def := range t.definitions {
		if def.kind == reflect.Interface || len(def.methods) == 0 {
			continue
		}
		ids := make(map[methodIdentity]int, len(def.methods))
		for _, method := range def.methods {
			ids[methodIdentity{method.name, method.pkg, method.pointer}] = method.function
		}
		if i < t.retained {
			methods, _, _, entries := concreteMethodSet(t.types[i])
			for j, method := range methods {
				id := ids[methodIdentity{method.Name, method.PkgPath, method.Pointer}]
				installed[id] = entries[j]
			}
			continue
		}
		methods := make([]reflectx.Method, len(def.methods))
		t.ctx.SetHasImethod(func(_ reflect.Type, m reflectx.Method) bool {
			for _, method := range def.methods {
				if method.name == m.Name && method.pkg == m.PkgPath {
					_, shared := installed[method.function]
					return method.hasInterface && !shared
				}
			}
			return false
		})
		for j, method := range def.methods {
			methods[j] = reflectx.Method{Name: method.name, PkgPath: method.pkg, Pointer: method.pointer, Type: t.types[method.typ-1], Func: callbacks[method.function-1]}
		}
		if err := t.ctx.SetMethodSet(t.types[i], methods, false); err != nil {
			return err
		}
		// SetMethodSet sorts methods and creates the wrappers, but shared methods
		// skip icall allocation. Install the first owner's guest-local entries.
		typ, ptyp := t.types[i], reflectx.PtrTo(t.types[i])
		values := runtimeMethods((*[2]unsafe.Pointer)(unsafe.Pointer(&typ))[1])
		pointers := runtimeMethods((*[2]unsafe.Pointer)(unsafe.Pointer(&ptyp))[1])
		var valueIndex int
		for j, method := range methods {
			id := ids[methodIdentity{method.Name, method.PkgPath, method.Pointer}]
			entry, shared := installed[id]
			if shared {
				pointers[j] = entry.pointer
			} else {
				entry.pointer = pointers[j]
			}
			if !method.Pointer {
				if shared {
					values[valueIndex] = entry.value
				} else {
					entry.value = values[valueIndex]
				}
				valueIndex++
			}
			installed[id] = entry
		}
	}
	return nil
}
