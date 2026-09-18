package state

import (
	"reflect"
	"runtime"
	"unsafe"

	"github.com/visualfc/xtype"
)

//go:linkname nativeReflectType reflect.toType
func nativeReflectType(unsafe.Pointer) reflect.Type

type nativeState struct {
	storage map[reflect.Type]uintptr
}

var makeFuncPC = reflect.MakeFunc(reflect.TypeFor[func()](), nil).Pointer()
var methodValuePC = reflect.ValueOf(reflect.Value{}).MethodByName("IsValid").Pointer()

// Go 1.26.6 reflect.makeFuncImpl on linux/amd64 and linux/arm64. Both
// architectures use a two-byte abi.IntArgRegBitmap. Transfer fn and ftyp's
// type reference; reflect.MakeFunc reconstructs the local call layout.
type makeFuncImpl struct {
	code    uintptr
	stack   unsafe.Pointer
	argLen  uintptr
	regPtrs [2]byte
	ftyp    unsafe.Pointer
	fn      func([]reflect.Value) []reflect.Value
}

type methodValueEnv struct {
	method   int
	receiver reflect.Value
}

// Go 1.26.6 reflect.methodValue on linux/amd64 and linux/arm64. Only env is
// transferred; receiver.Method reconstructs the local call layout.
type methodValueImpl struct {
	code    uintptr
	stack   unsafe.Pointer
	argLen  uintptr
	regPtrs [2]byte
	env     methodValueEnv
}

func methodValueStorage(obj reflect.Value) *methodValueImpl {
	if _, err := executableNativeMetadata(); err != nil {
		Failf("method value metadata: %w", err)
	}
	if !obj.CanAddr() {
		v := reflect.New(obj.Type()).Elem()
		v.Set(obj)
		obj = v
	}
	return *(**methodValueImpl)(obj.Addr().UnsafePointer())
}

func makeFuncStorage(obj reflect.Value) *makeFuncImpl {
	if obj.Pointer() != makeFuncPC {
		return nil
	}
	if _, err := executableNativeMetadata(); err != nil {
		Failf("MakeFunc metadata: %w", err)
	}
	if !obj.CanAddr() {
		v := reflect.New(obj.Type()).Elem()
		v.Set(obj)
		obj = v
	}
	return *(**makeFuncImpl)(obj.Addr().UnsafePointer())
}

func makeFuncCallback(obj reflect.Value) reflect.Value {
	impl := makeFuncStorage(obj)
	return reflect.ValueOf(&impl.fn).Elem()
}

// reflectxMethod adapts a restored function to SetMethods' local callback ABI.
// Its receiver retains the original function so a later Save can unwrap it,
// including when the caller starts a new State for the restored graph.
type reflectxMethod struct {
	function reflect.Value
}

func (m reflectxMethod) call(args []reflect.Value) []reflect.Value {
	if m.function.Type().IsVariadic() {
		return m.function.CallSlice(args)
	}
	return m.function.Call(args)
}

var reflectxMethodCallPC = reflect.ValueOf(reflectxMethod{}.call).Pointer()

func (ns *nativeState) originalFunction(obj reflect.Value) reflect.Value {
	if impl := makeFuncStorage(obj); impl != nil {
		callback := reflect.ValueOf(impl.fn)
		if callback.Pointer() == reflectxMethodCallPC {
			storage := ns.functionStorage(callback)
			return storage.Field(1).Interface().(reflectxMethod).function
		}
	}
	return obj
}

func (ns *nativeState) layout(pc uintptr, captureFree bool) reflect.Type {
	m, err := executableNativeMetadata()
	if err != nil {
		Failf("native closure metadata: %w", err)
	}
	typ, err := m.layout(pc, captureFree)
	if err != nil {
		Failf("native closure layout: %w", err)
	}
	if typ.NumField() == 1 {
		return typ
	}
	if ns.storage == nil {
		ns.storage = make(map[reflect.Type]uintptr)
	}
	ns.storage[typ] = pc
	return typ
}

func (ns *nativeState) functionStorage(obj reflect.Value) reflect.Value {
	if !obj.CanAddr() {
		v := reflect.New(obj.Type()).Elem()
		v.Set(obj)
		obj = v
	}
	storage := *(*unsafe.Pointer)(obj.Addr().UnsafePointer())
	m, err := executableNativeMetadata()
	if err != nil {
		Failf("native closure metadata: %w", err)
	}
	addr := uintptr(storage)
	typ := ns.layout(obj.Pointer(), addr >= m.funcStart && addr < m.funcEnd)
	return reflect.NewAt(typ, storage).Elem()
}

func (es *encodeState) encodeFunction(obj reflect.Value, dest *object) {
	f := &functionValue{}
	*dest = f
	if obj.IsNil() {
		return
	}
	obj = es.native.originalFunction(obj)
	pc := obj.Pointer()
	if impl := makeFuncStorage(obj); impl != nil {
		f.PC = uintValue(pc)
		f.Type = es.findType(nativeReflectType(impl.ftyp))
		es.resolve(reflect.ValueOf(&impl.fn), &f.Env)
		runtime.KeepAlive(obj)
		return
	}
	if pc == methodValuePC {
		f.PC = uintValue(pc)
		es.resolve(reflect.ValueOf(&methodValueStorage(obj).env), &f.Env)
		runtime.KeepAlive(obj)
		return
	}
	storage := es.native.functionStorage(obj)
	f.PC = uintValue(pc)
	if storage.NumField() != 1 {
		es.resolve(storage.Addr(), &f.Env)
	}
	runtime.KeepAlive(obj)
}

type decodedFunction struct {
	pc      uintptr
	storage reflect.Value
}

func (ds *decodeState) decodeFunction(obj reflect.Value, f *functionValue) {
	if obj.Kind() != reflect.Func {
		Failf("function record cannot be decoded into %v", obj.Type())
	}
	if len(f.Env.Dots) != 0 {
		Failf("invalid closure environment reference")
	}
	if f.PC == 0 && f.Env.Root == 0 {
		obj.SetZero()
		return
	}
	if f.PC == 0 {
		Failf("invalid closure PC or environment reference")
	}
	if f.Env.Root == 0 {
		ds.native.layout(uintptr(f.PC), true)
		storage := new(uintptr)
		*storage = uintptr(f.PC)
		*(*unsafe.Pointer)(obj.Addr().UnsafePointer()) = unsafe.Pointer(storage)
		return
	}
	if uintptr(f.PC) == makeFuncPC {
		signature := ds.findType(f.Type)
		typ := reflect.TypeFor[func([]reflect.Value) []reflect.Value]()
		callback := ds.register(&f.Env, typ)
		if callback.Type() != typ {
			Failf("MakeFunc callback has type %v, want %v", callback.Type(), typ)
		}
		fn, ok := ds.makeFuncs[callback]
		if !ok {
			// The callback may refer back to this function. Publish the
			// wrapper now and install callbacks after the graph is decoded.
			fn = reflect.MakeFunc(signature, nil)
			if ds.makeFuncs == nil {
				ds.makeFuncs = make(map[reflect.Value]reflect.Value)
			}
			ds.makeFuncs[callback] = fn
		}
		if fn.Type() != signature {
			Failf("MakeFunc reference changes internal signature from %v to %v", fn.Type(), signature)
		}
		// ixgo's linkname handling reinterprets the outer function type without
		// changing ftyp, e.g. func() *pkg.point viewed as func() *main.point.
		obj.Set(xtype.ConvertFuncValue(xtype.TypeOfType(obj.Type()), fn))
		return
	}
	if uintptr(f.PC) == methodValuePC {
		if _, err := executableNativeMetadata(); err != nil {
			Failf("method value metadata: %w", err)
		}
		typ := reflect.TypeFor[methodValueEnv]()
		env := ds.register(&f.Env, typ)
		if env.Type() != typ {
			Failf("method environment has type %v, want %v", env.Type(), typ)
		}
		fn, ok := ds.methodValues[env]
		if !ok {
			// A receiver may contain this function. Publish its storage before
			// decoding the receiver and fill the layout before AfterLoad runs.
			fn = reflect.New(obj.Type()).Elem()
			*(*unsafe.Pointer)(fn.Addr().UnsafePointer()) = unsafe.Pointer(&methodValueImpl{code: methodValuePC})
			if ds.methodValues == nil {
				ds.methodValues = make(map[reflect.Value]reflect.Value)
			}
			ds.methodValues[env] = fn
		}
		if !fn.Type().ConvertibleTo(obj.Type()) {
			Failf("method reference changes signature from %v to %v", fn.Type(), obj.Type())
		}
		obj.Set(fn.Convert(obj.Type()))
		return
	}
	typ := ds.native.layout(uintptr(f.PC), false)
	storage := ds.register(&f.Env, typ)
	if storage.Type() != typ {
		Failf("closure environment has type %v, want %v", storage.Type(), typ)
	}
	// The typed allocation keeps capture pointers visible to the local Go GC.
	// No allocator metadata or source heap addresses are imported.
	*(*unsafe.Pointer)(obj.Addr().UnsafePointer()) = storage.Addr().UnsafePointer()
	ds.functions = append(ds.functions, decodedFunction{uintptr(f.PC), storage})
}
