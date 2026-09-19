// Copyright 2026 The sandbox Authors.
// SPDX-License-Identifier: Apache-2.0

package reflectxtype

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"

	"github.com/goplus/reflectx"
)

// Open rebuilds a snapshot in this process. Static references require the same
// executable and type modules. The returned table does not retain data.
func Open(data []byte) (result *ReflectType, err error) {
	return open(data, nil)
}

// Open restores a returned snapshot, reusing the types from this export.
// Retained definitions must be unchanged; additional IDs describe new types.
func (s *Snapshot) Open(data []byte) (*ReflectType, error) {
	return open(data, s)
}

func open(data []byte, previous *Snapshot) (result *ReflectType, err error) {
	if runtime.Version() != "go1.26.6" {
		return nil, fmt.Errorf("reflectxtype requires go1.26.6, got %s", runtime.Version())
	}
	transferMu.Lock()
	defer transferMu.Unlock()
	// Invalid input and constructor errors cannot publish a partially built table.
	defer func() {
		if failure := recover(); failure != nil {
			result = nil
			err = fmt.Errorf("open reflectx types: %v", failure)
		}
	}()
	r := typeReader(data)
	n := r.count()
	if uint64(n) > math.MaxUint32 {
		return nil, fmt.Errorf("too many reflectx types")
	}
	d := importer{
		definitions: make([]definition, n),
		types:       make([]reflect.Type, n),
		resolving:   make([]bool, n),
		done:        make([]bool, n),
		mocks:       make([]reflect.Type, n),
		mocking:     make([]bool, n),
		ctx:         reflectx.NewContext(),
	}
	entries := make([][]byte, n)
	for i := range d.definitions {
		length := r.count()
		entry := typeReader(r[:length])
		entries[i] = bytes.Clone(entry)
		d.parse(i, &entry)
		if len(entry) != 0 {
			return nil, fmt.Errorf("trailing data for reflectx type ID %d", i+1)
		}
		r = r[length:]
	}
	if len(r) != 0 {
		return nil, fmt.Errorf("trailing reflectx type table data")
	}
	var retained int
	if previous != nil {
		old := typeReader(previous.Data)
		retained = old.count()
		if retained > n || len(previous.IDs) != retained {
			return nil, fmt.Errorf("returned reflectx type table lost retained IDs")
		}
		for i := range retained {
			length := old.count()
			if !bytes.Equal(entries[i], old[:length]) {
				return nil, fmt.Errorf("returned reflectx type ID %d changed definition", i+1)
			}
			old = old[length:]
		}
		if len(old) != 0 {
			return nil, fmt.Errorf("trailing retained reflectx type table data")
		}
		for typ, id := range previous.IDs {
			if id == 0 || uint64(id) > uint64(retained) {
				return nil, fmt.Errorf("invalid retained reflectx type ID %d", id)
			}
			d.types[id-1], d.done[id-1] = typ, true
		}
	}
	// Callback IDs index the complete method table, not the bytes remaining
	// in an individual type record. For example, its last byte may be ID 1.
	functions := make([]method, d.methodCount)
	var methodCount int
	for _, def := range d.definitions {
		if def.kind == reflect.Interface {
			continue
		}
		for _, method := range def.methods {
			if method.function > d.methodCount {
				return nil, fmt.Errorf("invalid method function index %d", method.function)
			}
			previous := functions[method.function-1]
			if previous.function != 0 && previous != method {
				return nil, fmt.Errorf("invalid method function index %d: inconsistent shared method", method.function)
			}
			functions[method.function-1] = method
			methodCount = max(methodCount, method.function)
		}
	}
	for i := range methodCount {
		if functions[i].function == 0 {
			return nil, fmt.Errorf("missing method function index %d", i+1)
		}
	}
	d.methodCount = methodCount
	// Named types must keep their identity when a dependency refers back to them.
	// The mock has the final storage layout; e.g. Node{N int; Next *Node} uses
	// {N int; Next *struct{}} until its fields can point at the completed types.
	for i, def := range d.definitions {
		if !d.done[i] && def.name != "" {
			d.types[i] = reflectx.NamedTypeOf(def.pkg, def.name, d.mock(uint32(i+1)))
			if def.kind != reflect.Interface && len(def.methods) != 0 {
				d.types[i] = d.reserveMethods(d.types[i], def.methods)
			}
		}
	}
	for i := range d.definitions {
		d.resolve(uint32(i + 1))
	}
	return &ReflectType{types: d.types, entries: entries, definitions: d.definitions, ctx: d.ctx, methodCount: d.methodCount, retained: retained}, nil
}

type definition struct {
	kind        reflect.Kind
	name, pkg   string
	size, align uint64
	comparable  bool
	elem, key   uint32
	length      int
	direction   reflect.ChanDir
	variadic    bool
	in, out     []uint32
	fields      []field
	methods     []method
}

type field struct {
	name, pkg, tag string
	anonymous      bool
	offset         uint64
	typ            uint32
}

type method struct {
	name, pkg    string
	typ          uint32
	pointer      bool
	hasInterface bool
	function     int
}

type importer struct {
	definitions     []definition
	types           []reflect.Type
	resolving, done []bool
	mocks           []reflect.Type
	mocking         []bool
	ctx             *reflectx.Context
	methodCount     int
}

func (d *importer) parse(index int, r *typeReader) {
	tag := r.uint()
	if tag == 0 {
		module, offset := r.uint(), r.uint()
		if module > math.MaxUint32 {
			panic(fmt.Errorf("invalid static type module %d", module))
		}
		d.types[index] = staticTypes().byLocation[staticLocation{uint32(module), offset}]
		if d.types[index] == nil {
			panic(fmt.Errorf("unavailable static type module=%d offset=%#x", module, offset))
		}
		d.done[index] = true
		return
	}
	if tag < dynamic {
		if tag >= uint64(len(builtinTypes)) || builtinTypes[tag] == nil {
			panic(fmt.Errorf("invalid builtin kind %d", tag))
		}
		d.types[index], d.done[index] = builtinTypes[tag], true
		return
	}
	hasMethods := tag&concreteMethods != 0
	tag &^= concreteMethods
	if tag <= dynamic || tag > dynamic+uint64(reflect.UnsafePointer) {
		panic(fmt.Errorf("invalid dynamic kind %d", tag))
	}
	def := &d.definitions[index]
	def.kind = reflect.Kind(tag - dynamic)
	def.name, def.pkg = r.string(), r.string()
	if def.name == "" && def.pkg != "" {
		panic(fmt.Errorf("unnamed type has package path %q", def.pkg))
	}
	def.size, def.align, def.comparable = r.uint(), r.uint(), r.flag()
	switch def.kind {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		def.elem = d.readID(r)
		if def.kind == reflect.Array {
			length := r.uint()
			if length > uint64(math.MaxInt) {
				panic(fmt.Errorf("array length %d overflows int", length))
			}
			def.length = int(length)
		} else if def.kind == reflect.Chan {
			dir := r.uint()
			if dir < uint64(reflect.RecvDir) || dir > uint64(reflect.BothDir) {
				panic(fmt.Errorf("invalid channel direction %d", dir))
			}
			def.direction = reflect.ChanDir(dir)
		}
	case reflect.Map:
		def.key, def.elem = d.readID(r), d.readID(r)
	case reflect.Func:
		def.variadic = r.flag()
		def.in = make([]uint32, r.count())
		for i := range def.in {
			def.in[i] = d.readID(r)
		}
		def.out = make([]uint32, r.count())
		for i := range def.out {
			def.out[i] = d.readID(r)
		}
		if len(def.in) > math.MaxUint16 || len(def.out) > math.MaxInt16 || (def.variadic && len(def.in) == 0) {
			panic(fmt.Errorf("invalid function arity"))
		}
	case reflect.Struct:
		def.fields = make([]field, r.count())
		for i := range def.fields {
			def.fields[i] = field{r.string(), r.string(), r.string(), r.flag(), r.uint(), d.readID(r)}
		}
	case reflect.Interface:
		def.methods = make([]method, r.count())
		for i := range def.methods {
			def.methods[i] = method{name: r.string(), pkg: r.string(), typ: d.readID(r)}
		}
	default:
		if def.name == "" || builtinTypes[def.kind] == nil {
			panic(fmt.Errorf("invalid named primitive kind %v", def.kind))
		}
	}
	if hasMethods {
		if def.kind == reflect.Interface || def.kind == reflect.Pointer && def.name == "" {
			panic(fmt.Errorf("invalid concrete method owner at type ID %d", index+1))
		}
		def.methods = make([]method, r.count())
		d.methodCount += len(def.methods)
		for i := range def.methods {
			m := method{name: r.string(), pkg: r.string(), pointer: r.flag(), hasInterface: r.flag(), typ: d.readID(r)}
			function := r.uint()
			if function == 0 || function > math.MaxInt {
				panic(fmt.Errorf("invalid method function index %d", function))
			}
			m.function = int(function)
			def.methods[i] = m
		}
	}
}

func (d *importer) reserveMethods(typ reflect.Type, methods []method) reflect.Type {
	var values int
	for _, method := range methods {
		if !method.pointer {
			values++
		}
	}
	return d.ctx.NewMethodSet(typ, values, len(methods))
}

func (d *importer) readID(r *typeReader) uint32 {
	id := r.uint()
	if id == 0 || id > uint64(len(d.types)) {
		panic(fmt.Errorf("unknown reflectx type ID %d", id))
	}
	return uint32(id)
}

func (d *importer) resolve(id uint32) reflect.Type {
	i := id - 1
	if d.done[i] {
		return d.types[i]
	}
	if d.resolving[i] {
		if d.types[i] != nil {
			return d.types[i]
		}
		panic(fmt.Errorf("recursive unnamed type at ID %d", id))
	}
	d.resolving[i] = true
	def := &d.definitions[i]
	typ := d.construct(def, d.reference)
	if def.name != "" {
		reflectx.SetUnderlying(d.types[i], typ)
		typ = d.types[i]
	} else if def.kind != reflect.Interface && len(def.methods) != 0 {
		typ = d.reserveMethods(typ, def.methods)
	}
	if uint64(typ.Size()) != def.size || uint64(typ.Align()) != def.align || typ.Comparable() != def.comparable {
		panic(fmt.Errorf("layout mismatch for reflectx type ID %d (%v)", id, typ))
	}
	if typ.Name() != def.name || typ.PkgPath() != def.pkg {
		panic(fmt.Errorf("identity mismatch for reflectx type ID %d", id))
	}
	if def.kind == reflect.Struct {
		for j, f := range def.fields {
			got := typ.Field(j)
			if uint64(got.Offset) != f.offset || got.PkgPath != f.pkg || got.Name != f.name || got.Anonymous != f.anonymous || string(got.Tag) != f.tag {
				panic(fmt.Errorf("field mismatch for reflectx type ID %d field %d", id, j))
			}
		}
	} else if def.kind == reflect.Interface {
		if typ.NumMethod() != len(def.methods) {
			panic(fmt.Errorf("interface method count mismatch at ID %d", id))
		}
		for j, m := range def.methods {
			got := typ.Method(j)
			if got.Name != m.name || got.PkgPath != m.pkg || got.Type != d.types[m.typ-1] {
				panic(fmt.Errorf("interface method mismatch for reflectx type ID %d method %d", id, j))
			}
		}
	}
	d.types[i], d.done[i], d.resolving[i] = typ, true, false
	return typ
}

// Named dependencies already have their final identity and storage layout.
// Following their definitions here would make *Node -> Node -> *Node depend
// on which record happened to be exported first.
func (d *importer) reference(id uint32) reflect.Type {
	if d.definitions[id-1].name != "" {
		return d.types[id-1]
	}
	return d.resolve(id)
}

func (d *importer) construct(def *definition, resolve func(uint32) reflect.Type) reflect.Type {
	switch def.kind {
	case reflect.Pointer:
		return reflectx.PtrTo(resolve(def.elem))
	case reflect.Slice:
		return reflect.SliceOf(resolve(def.elem))
	case reflect.Array:
		return reflect.ArrayOf(def.length, resolve(def.elem))
	case reflect.Chan:
		return reflect.ChanOf(def.direction, resolve(def.elem))
	case reflect.Map:
		return reflect.MapOf(resolve(def.key), resolve(def.elem))
	case reflect.Func:
		in, out := make([]reflect.Type, len(def.in)), make([]reflect.Type, len(def.out))
		for i, id := range def.in {
			in[i] = resolve(id)
		}
		for i, id := range def.out {
			out[i] = resolve(id)
		}
		if def.variadic {
			// FuncOf renders the last slice's element, even when that slice
			// is named. Finish Args []Node before recording ...Node.
			in[len(in)-1] = d.resolve(def.in[len(in)-1])
		}
		return reflect.FuncOf(in, out, def.variadic)
	case reflect.Struct:
		fields := make([]reflect.StructField, len(def.fields))
		for i, f := range def.fields {
			fields[i] = reflect.StructField{Name: f.name, PkgPath: f.pkg, Tag: reflect.StructTag(f.tag), Anonymous: f.anonymous, Type: resolve(f.typ)}
		}
		return structOf(fields)
	case reflect.Interface:
		methods := make([]reflect.Method, len(def.methods))
		for i, m := range def.methods {
			typ := resolve(m.typ)
			if typ.Kind() != reflect.Func {
				panic(fmt.Errorf("interface method %s is not a function", m.name))
			}
			methods[i] = reflect.Method{Name: m.name, PkgPath: m.pkg, Type: typ}
		}
		// Context's interface cache is keyed by display strings, which omit
		// private method package paths. Keep separate definitions separate.
		return reflectx.NewContext().InterfaceOf(nil, methods)
	default:
		return builtinTypes[def.kind]
	}
}

// mock cuts references through fixed-size descriptors while following inline
// storage. Function arity must match because NamedTypeOf allocates its trailing
// parameter array here; SetUnderlying cannot grow that allocation later.
func (d *importer) mock(id uint32) reflect.Type {
	i := id - 1
	if d.done[i] {
		return d.types[i]
	}
	if d.mocks[i] != nil {
		return d.mocks[i]
	}
	if d.mocking[i] {
		panic(fmt.Errorf("recursive inline storage at type ID %d", id))
	}
	d.mocking[i] = true
	def := &d.definitions[i]
	var typ reflect.Type
	switch def.kind {
	case reflect.Pointer:
		typ = reflect.TypeFor[*struct{}]()
	case reflect.Slice:
		typ = reflect.TypeFor[[]struct{}]()
	case reflect.Map:
		typ = reflect.TypeFor[map[struct{}]struct{}]()
	case reflect.Chan:
		typ = reflect.ChanOf(def.direction, reflect.TypeFor[struct{}]())
	case reflect.Interface:
		methods := make([]reflect.Method, len(def.methods))
		for i, m := range def.methods {
			methods[i] = reflect.Method{Name: m.name, PkgPath: m.pkg, Type: reflect.TypeFor[func()]()}
		}
		typ = reflectx.NewContext().InterfaceOf(nil, methods)
	case reflect.Func:
		in, out := make([]reflect.Type, len(def.in)), make([]reflect.Type, len(def.out))
		for i := range in {
			in[i] = reflect.TypeFor[struct{}]()
		}
		for i := range out {
			out[i] = reflect.TypeFor[struct{}]()
		}
		if def.variadic {
			in[len(in)-1] = reflect.TypeFor[[]struct{}]()
		}
		typ = reflect.FuncOf(in, out, def.variadic)
	case reflect.Struct:
		fields := make([]reflect.StructField, len(def.fields))
		for i, f := range def.fields {
			fields[i] = reflect.StructField{Name: f.name, PkgPath: f.pkg, Tag: reflect.StructTag(f.tag), Type: d.mock(f.typ)}
		}
		typ = structOf(fields)
	case reflect.Array:
		typ = reflect.ArrayOf(def.length, d.mock(def.elem))
	default:
		typ = builtinTypes[def.kind]
	}
	if uint64(typ.Size()) != def.size || uint64(typ.Align()) != def.align || typ.Comparable() != def.comparable {
		panic(fmt.Errorf("invalid storage layout at type ID %d", id))
	}
	d.mocks[i], d.mocking[i] = typ, false
	return typ
}

type typeReader []byte

func (r *typeReader) uint() uint64 {
	value, n := binary.Uvarint(*r)
	if n == 0 {
		panic(io.ErrUnexpectedEOF)
	}
	if n < 0 {
		panic(fmt.Errorf("reflectx type integer overflow"))
	}
	*r = (*r)[n:]
	return value
}

func (r *typeReader) count() int {
	n := r.uint()
	if n > uint64(len(*r)) {
		panic(io.ErrUnexpectedEOF)
	}
	return int(n)
}

func (r *typeReader) string() string {
	n := r.count()
	value := string((*r)[:n])
	*r = (*r)[n:]
	return value
}

func (r *typeReader) flag() bool {
	value := r.uint()
	if value > 1 {
		panic(fmt.Errorf("invalid reflectx type flag %d", value))
	}
	return value == 1
}
