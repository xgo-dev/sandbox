// Copyright 2020 The gVisor Authors.
//
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

// Object representations and their encoding are adapted from gVisor pkg/state/wire
// at d1e35511e5a4. They are private to state; transport reads and writes byte slices.
package state

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// object is a generic object.
type object interface {
	// save saves the given object.
	//
	// Panic is used for error control flow.
	save(*writer)

	// load loads a new object of the given type.
	//
	// Panic is used for error control flow.
	load(*reader) object
}

// boolValue is a boolean.
type boolValue bool

// loadBool loads an object of type boolValue.
func loadBool(r *reader) boolValue {
	b := loadUint(r)
	return boolValue(b == 1)
}

// save implements object.save.
func (b boolValue) save(w *writer) {
	var v uintValue
	if b {
		v = 1
	} else {
		v = 0
	}
	v.save(w)
}

// load implements object.load.
func (boolValue) load(r *reader) object { return loadBool(r) }

// intValue is a signed integer.
//
// This uses varint encoding.
type intValue int64

// loadInt loads an object of type intValue.
func loadInt(r *reader) intValue {
	u := loadUint(r)
	x := intValue(u >> 1)
	if u&1 != 0 {
		x = ^x
	}
	return x
}

// save implements object.save.
func (i intValue) save(w *writer) {
	u := uintValue(i) << 1
	if i < 0 {
		u = ^u
	}
	u.save(w)
}

// load implements object.load.
func (intValue) load(r *reader) object { return loadInt(r) }

// uintValue is an unsigned integer.
type uintValue uint64

// loadUint loads an object of type uintValue.
func loadUint(r *reader) uintValue {
	v, n := binary.Uvarint(r.mem[r.pos:])
	if n == 0 {
		panic(io.ErrUnexpectedEOF)
	}
	if n < 0 {
		panic(fmt.Errorf("integer overflow"))
	}
	r.pos += n
	return uintValue(v)
}

// save implements object.save.
func (u uintValue) save(w *writer) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(u))
	w.writeBytes(buf[:n])
}

// load implements object.load.
func (uintValue) load(r *reader) object { return loadUint(r) }

// float32Value is a 32-bit floating point number.
type float32Value float32

// loadFloat32 loads an object of type float32Value.
func loadFloat32(r *reader) float32Value {
	n := loadUint(r)
	return float32Value(math.Float32frombits(uint32(n)))
}

// save implements object.save.
func (f float32Value) save(w *writer) {
	n := uintValue(math.Float32bits(float32(f)))
	n.save(w)
}

// load implements object.load.
func (float32Value) load(r *reader) object { return loadFloat32(r) }

// float64Value is a 64-bit floating point number.
type float64Value float64

// loadFloat64 loads an object of type float64Value.
func loadFloat64(r *reader) float64Value {
	n := loadUint(r)
	return float64Value(math.Float64frombits(uint64(n)))
}

// save implements object.save.
func (f float64Value) save(w *writer) {
	n := uintValue(math.Float64bits(float64(f)))
	n.save(w)
}

// load implements object.load.
func (float64Value) load(r *reader) object { return loadFloat64(r) }

// complex64Value is a 64-bit complex number.
type complex64Value complex128

// loadComplex64 loads an object of type complex64Value.
func loadComplex64(r *reader) complex64Value {
	re := loadFloat32(r)
	im := loadFloat32(r)
	return complex64Value(complex(float32(re), float32(im)))
}

// save implements object.save.
func (c *complex64Value) save(w *writer) {
	re := float32Value(real(*c))
	im := float32Value(imag(*c))
	re.save(w)
	im.save(w)
}

// load implements object.load.
func (*complex64Value) load(r *reader) object {
	c := loadComplex64(r)
	return &c
}

// complex128Value is a 128-bit complex number.
type complex128Value complex128

// loadComplex128 loads an object of type complex128Value.
func loadComplex128(r *reader) complex128Value {
	re := loadFloat64(r)
	im := loadFloat64(r)
	return complex128Value(complex(float64(re), float64(im)))
}

// save implements object.save.
func (c *complex128Value) save(w *writer) {
	re := float64Value(real(*c))
	im := float64Value(imag(*c))
	re.save(w)
	im.save(w)
}

// load implements object.load.
func (*complex128Value) load(r *reader) object {
	c := loadComplex128(r)
	return &c
}

// stringValue is a string.
type stringValue string

// loadString loads an object of type stringValue.
func loadString(r *reader) stringValue {
	l := loadUint(r)
	return stringValue(r.readBytes(uint64(l)))
}

// save implements object.save.
func (s *stringValue) save(w *writer) {
	l := uintValue(len(*s))
	l.save(w)
	w.writeString(string(*s))
}

// load implements object.load.
func (*stringValue) load(r *reader) object {
	s := loadString(r)
	return &s
}

// dot selects an array element, array range, or struct field.
type dot interface {
	isDot()
}

// index is a reference resolution.
type index uint32

func (index) isDot() {}

type arrayRange struct {
	start, length uintValue
}

func (arrayRange) isDot() {}

// Array indices fit in uint32; the next value identifies a range.
const arrayRangeTag intValue = 1 << 32

// fieldName is a reference resolution.
type fieldName string

func (*fieldName) isDot() {}

// refValue is a reference to an object.
type refValue struct {
	// Root is the root object.
	Root uintValue

	// Dots is the set of traversals required from the Root object above.
	// Note that this will be stored in reverse order for efficiency.
	Dots []dot

	// Type is the root object's type when Dots is nonempty or the pointer
	// targets a different type through a Go pointer conversion.
	Type typeSpec
}

// loadRef loads an object of type refValue (abstract).
func loadRef(r *reader) refValue {
	ref := refValue{
		Root: loadUint(r),
	}
	header := loadUint(r)
	l := header >> 1
	ref.Dots = make([]dot, l)
	for i := 0; i < int(l); i++ {
		// Field names use negative lengths; non-negative values select
		// array indices or the range tag.
		d := loadInt(r)
		if d == arrayRangeTag {
			ref.Dots[i] = arrayRange{start: loadUint(r), length: loadUint(r)}
			continue
		}
		if d >= 0 {
			ref.Dots[i] = index(d)
			continue
		}
		fieldName := fieldName(r.readBytes(uint64(-d)))
		ref.Dots[i] = &fieldName
	}
	if header&1 != 0 {
		ref.Type = loadTypeSpec(r)
	}
	return ref
}

// save implements object.save.
func (r *refValue) save(w *writer) {
	r.Root.save(w)
	// The low bit marks an explicit root type, independently of the path.
	header := uintValue(len(r.Dots)) << 1
	if r.Type != nil {
		header |= 1
	}
	header.save(w)
	for _, d := range r.Dots {
		switch x := d.(type) {
		case index:
			i := intValue(x)
			i.save(w)
		case arrayRange:
			arrayRangeTag.save(w)
			x.start.save(w)
			x.length.save(w)
		case *fieldName:
			d := intValue(-len(*x))
			d.save(w)
			w.writeString(string(*x))
		default:
			panic("unknown dot implementation")
		}
	}
	if r.Type != nil {
		saveTypeSpec(w, r.Type)
	}
}

// load implements object.load.
func (*refValue) load(r *reader) object {
	ref := loadRef(r)
	return &ref
}

// nilValue is a primitive zero value of any type.
type nilValue struct{}

// loadNil loads an object of type nilValue.
func loadNil(r *reader) nilValue {
	return nilValue{}
}

// save implements object.save.
func (nilValue) save(w *writer) {}

// load implements object.load.
func (nilValue) load(r *reader) object { return loadNil(r) }

// sliceValue is a slice value.
type sliceValue struct {
	Length   uintValue
	Capacity uintValue
	Ref      refValue
}

// Capacity travels with the reference so every alias can immediately use the
// final channel, even when its queued values have not been decoded yet.
type channelValue struct {
	Capacity uintValue
	Ref      refValue
}

func (c *channelValue) save(w *writer) {
	c.Capacity.save(w)
	c.Ref.save(w)
}

func (*channelValue) load(r *reader) object {
	return &channelValue{Capacity: loadUint(r), Ref: loadRef(r)}
}

type channelData struct {
	Closed boolValue
	Values arrayValue
}

func (c *channelData) save(w *writer) {
	c.Closed.save(w)
	c.Values.save(w)
}

func (*channelData) load(r *reader) object {
	return &channelData{Closed: loadBool(r), Values: loadArray(r)}
}

// functionValue identifies code in the same executable and the closure storage
// in the object graph. Env references preserve shared and recursive closures.
// A nonzero PC with Env.Root == 0 has no captures; Load creates only a PC word.
// For reflect.makeFuncStub, Env refers to the callback slot and Type records
// the internal signature, which may differ from the outer function type.
type functionValue struct {
	PC   uintValue
	Env  refValue
	Type typeSpec
}

func (f *functionValue) save(w *writer) {
	f.PC.save(w)
	f.Env.save(w)
	boolValue(f.Type != nil).save(w)
	if f.Type != nil {
		saveTypeSpec(w, f.Type)
	}
}

func (*functionValue) load(r *reader) object {
	f := &functionValue{PC: loadUint(r), Env: loadRef(r)}
	if loadBool(r) {
		f.Type = loadTypeSpec(r)
	}
	return f
}

// reflectTypeValue represents a type itself, without traversing *reflect.rtype.
type reflectTypeValue struct {
	Type typeSpec
}

func (t *reflectTypeValue) save(w *writer) { saveTypeSpec(w, t.Type) }

func (*reflectTypeValue) load(r *reader) object {
	return &reflectTypeValue{Type: loadTypeSpec(r)}
}

// reflectedValue saves the represented value, not reflect.Value's runtime
// fields. Addressable values encode their address so aliases remain shared.
type reflectedValue struct {
	Type        typeSpec
	Value       object
	Addressable bool
	ReadOnly    uint64
}

func (v *reflectedValue) save(w *writer) {
	saveTypeSpec(w, v.Type)
	boolValue(v.Addressable).save(w)
	uintValue(v.ReadOnly).save(w)
	saveObject(w, v.Value)
}

func (*reflectedValue) load(r *reader) object {
	typ := loadTypeSpec(r)
	addressable := loadBool(r)
	readOnly := uint64(loadUint(r))
	if readOnly&^reflectValueReadOnlyMask != 0 {
		Failf("invalid reflect.Value read-only flags %#x", readOnly)
	}
	return &reflectedValue{Type: typ, Value: loadObject(r), Addressable: bool(addressable), ReadOnly: readOnly}
}

// loadSlice loads an object of type sliceValue.
func loadSlice(r *reader) sliceValue {
	return sliceValue{
		Length:   loadUint(r),
		Capacity: loadUint(r),
		Ref:      loadRef(r),
	}
}

// save implements object.save.
func (s *sliceValue) save(w *writer) {
	s.Length.save(w)
	s.Capacity.save(w)
	s.Ref.save(w)
}

// load implements object.load.
func (*sliceValue) load(r *reader) object {
	s := loadSlice(r)
	return &s
}

// arrayValue is an array value.
type arrayValue struct {
	Contents []object
}

// loadArray loads an object of type arrayValue.
func loadArray(r *reader) arrayValue {
	l := loadUint(r)
	if l == 0 {
		// Note that there isn't a single object available to encode
		// the type of, so we need this additional branch.
		return arrayValue{}
	}
	// All the objects here have the same type, so use dynamic dispatch
	// only once. All other objects will automatically take the same type
	// as the first object.
	contents := make([]object, l)
	v := loadObject(r)
	contents[0] = v
	for i := 1; i < int(l); i++ {
		contents[i] = v.load(r)
	}
	return arrayValue{
		Contents: contents,
	}
}

// save implements object.save.
func (a *arrayValue) save(w *writer) {
	l := uintValue(len(a.Contents))
	l.save(w)
	if l == 0 {
		// See LoadArray.
		return
	}
	// See above.
	saveObject(w, a.Contents[0])
	for i := 1; i < int(l); i++ {
		a.Contents[i].save(w)
	}
}

// load implements object.load.
func (*arrayValue) load(r *reader) object {
	a := loadArray(r)
	return &a
}

// rawArrayValue holds numeric array bytes in the executable's native layout.
// It has no references; the enclosing object keeps its existing object ID.
type rawArrayValue struct {
	Data []byte
}

func (a *rawArrayValue) save(w *writer) {
	uintValue(len(a.Data)).save(w)
	w.writeBytes(a.Data)
}

func (*rawArrayValue) load(r *reader) object {
	return &rawArrayValue{Data: r.readBytes(uint64(loadUint(r)))}
}

// mapValue is a map value.
type mapValue struct {
	Keys   []object
	Values []object
}

// loadMap loads an object of type mapValue.
func loadMap(r *reader) mapValue {
	l := loadUint(r)
	if l == 0 {
		// See LoadArray.
		return mapValue{}
	}
	// See type dispatch notes in arrayValue.
	keys := make([]object, l)
	values := make([]object, l)
	k := loadObject(r)
	v := loadObject(r)
	keys[0] = k
	values[0] = v
	for i := 1; i < int(l); i++ {
		keys[i] = k.load(r)
		values[i] = v.load(r)
	}
	return mapValue{
		Keys:   keys,
		Values: values,
	}
}

// save implements object.save.
func (m *mapValue) save(w *writer) {
	l := uintValue(len(m.Keys))
	if int(l) != len(m.Values) {
		panic(fmt.Sprintf("mismatched keys (%d) and values (%d)", len(m.Keys), len(m.Values)))
	}
	l.save(w)
	if l == 0 {
		// See LoadArray.
		return
	}
	// See above.
	saveObject(w, m.Keys[0])
	saveObject(w, m.Values[0])
	for i := 1; i < int(l); i++ {
		m.Keys[i].save(w)
		m.Values[i].save(w)
	}
}

// load implements object.load.
func (*mapValue) load(r *reader) object {
	m := loadMap(r)
	return &m
}

// typeSpec is a type dereference.
type typeSpec interface {
	isTypeSpec()
}

// typeSpecID is a concrete type ID.
type typeSpecID uintValue

func (typeSpecID) isTypeSpec() {}

// reflectedType is filled from Snapshot.IDs after graph traversal. It is not
// a custom StateSave schema ID, which continues to use typeSpecID.
type reflectedType struct {
	ID       uintValue
	reflectx bool
}

func (*reflectedType) isTypeSpec() {}

// pointerType is a pointer type.
type pointerType struct {
	Type typeSpec
}

func (*pointerType) isTypeSpec() {}

// arrayType is an array type.
type arrayType struct {
	Count uintValue
	Type  typeSpec
}

func (*arrayType) isTypeSpec() {}

// sliceType is a slice type.
type sliceType struct {
	Type typeSpec
}

func (*sliceType) isTypeSpec() {}

// mapType is a map type.
type mapType struct {
	Key   typeSpec
	Value typeSpec
}

func (*mapType) isTypeSpec() {}

// nilType is an empty type.
type nilType struct{}

func (nilType) isTypeSpec() {}

// closureType names a closure storage type recovered from the executable.
type closureType uintValue

func (closureType) isTypeSpec() {}

// typeSpec types.
//
// These use a distinct encoding on the wire, as they are used only in the
// interface object. They are decoded through the dedicated loadTypeSpec and
// saveTypeSpec functions.
const (
	typeSpecTypeID uintValue = iota
	typeSpecPointer
	typeSpecArray
	typeSpecSlice
	typeSpecMap
	typeSpecNil
	typeSpecClosure
	typeSpecReflected
	typeSpecReflectx
)

// loadTypeSpec loads typeSpec values.
func loadTypeSpec(r *reader) typeSpec {
	switch hdr := loadUint(r); hdr {
	case typeSpecTypeID:
		return typeSpecID(loadUint(r))
	case typeSpecPointer:
		return &pointerType{
			Type: loadTypeSpec(r),
		}
	case typeSpecArray:
		return &arrayType{
			Count: loadUint(r),
			Type:  loadTypeSpec(r),
		}
	case typeSpecSlice:
		return &sliceType{
			Type: loadTypeSpec(r),
		}
	case typeSpecMap:
		return &mapType{
			Key:   loadTypeSpec(r),
			Value: loadTypeSpec(r),
		}
	case typeSpecNil:
		return nilType{}
	case typeSpecClosure:
		return closureType(loadUint(r))
	case typeSpecReflected:
		return &reflectedType{ID: loadUint(r)}
	case typeSpecReflectx:
		return &reflectedType{ID: loadUint(r), reflectx: true}
	default:
		// This is not a valid stream?
		panic(fmt.Errorf("unknown header: %d", hdr))
	}
}

// saveTypeSpec saves typeSpec values.
func saveTypeSpec(w *writer, t typeSpec) {
	switch x := t.(type) {
	case typeSpecID:
		typeSpecTypeID.save(w)
		uintValue(x).save(w)
	case *pointerType:
		typeSpecPointer.save(w)
		saveTypeSpec(w, x.Type)
	case *arrayType:
		typeSpecArray.save(w)
		x.Count.save(w)
		saveTypeSpec(w, x.Type)
	case *sliceType:
		typeSpecSlice.save(w)
		saveTypeSpec(w, x.Type)
	case *mapType:
		typeSpecMap.save(w)
		saveTypeSpec(w, x.Key)
		saveTypeSpec(w, x.Value)
	case nilType:
		typeSpecNil.save(w)
	case closureType:
		typeSpecClosure.save(w)
		uintValue(x).save(w)
	case *reflectedType:
		if x.reflectx {
			typeSpecReflectx.save(w)
		} else {
			typeSpecReflected.save(w)
		}
		x.ID.save(w)
	default:
		// This should not happen?
		panic(fmt.Errorf("unknown type %T", t))
	}
}

// interfaceValue is an interface value.
type interfaceValue struct {
	Type  typeSpec
	Value object
}

// loadInterface loads an object of type interfaceValue.
func loadInterface(r *reader) interfaceValue {
	return interfaceValue{
		Type:  loadTypeSpec(r),
		Value: loadObject(r),
	}
}

// save implements object.save.
func (i *interfaceValue) save(w *writer) {
	saveTypeSpec(w, i.Type)
	saveObject(w, i.Value)
}

// load implements object.load.
func (*interfaceValue) load(r *reader) object {
	i := loadInterface(r)
	return &i
}

// typeDescriptor is type information.
type typeDescriptor struct {
	Name   string
	Fields []string
}

// loadType loads an object of type typeDescriptor.
func loadType(r *reader) typeDescriptor {
	name := string(loadString(r))
	l := loadUint(r)
	fields := make([]string, l)
	for i := 0; i < int(l); i++ {
		fields[i] = string(loadString(r))
	}
	return typeDescriptor{
		Name:   name,
		Fields: fields,
	}
}

// save implements object.save.
func (t *typeDescriptor) save(w *writer) {
	s := stringValue(t.Name)
	s.save(w)
	l := uintValue(len(t.Fields))
	l.save(w)
	for i := 0; i < int(l); i++ {
		s := stringValue(t.Fields[i])
		s.save(w)
	}
}

// load implements object.load.
func (*typeDescriptor) load(r *reader) object {
	t := loadType(r)
	return &t
}

// multipleObjects is a special type for serializing multiple objects.
type multipleObjects []object

// loadMultipleObjects loads a series of objects.
func loadMultipleObjects(r *reader) multipleObjects {
	l := loadUint(r)
	m := make(multipleObjects, l)
	for i := 0; i < int(l); i++ {
		m[i] = loadObject(r)
	}
	return m
}

// save implements object.save.
func (m *multipleObjects) save(w *writer) {
	l := uintValue(len(*m))
	l.save(w)
	for i := 0; i < int(l); i++ {
		saveObject(w, (*m)[i])
	}
}

// load implements object.load.
func (*multipleObjects) load(r *reader) object {
	m := loadMultipleObjects(r)
	return &m
}

// noObjects represents no objects.
type noObjects struct{}

// loadNoObjects loads a sentinel.
func loadNoObjects(r *reader) noObjects { return noObjects{} }

// save implements object.save.
func (noObjects) save(w *writer) {}

// load implements object.load.
func (noObjects) load(r *reader) object { return loadNoObjects(r) }

// structValue is a basic composite value.
type structValue struct {
	TypeID typeSpecID
	fields object // Optionally noObjects or *multipleObjects.
}

// Field returns a pointer to the given field slot.
//
// This must be called after Alloc.
func (s *structValue) Field(i int) *object {
	if fields, ok := s.fields.(*multipleObjects); ok {
		return &((*fields)[i])
	}
	if _, ok := s.fields.(noObjects); ok {
		// Alloc may be optionally called; can't call twice.
		panic("Field called inappropriately, wrong Alloc?")
	}
	return &s.fields
}

// Alloc allocates the given number of fields.
//
// This must be called before Add and saveObject.
//
// Precondition: slots must be positive.
func (s *structValue) Alloc(slots int) {
	switch {
	case slots == 0:
		s.fields = noObjects{}
	case slots == 1:
		// Leave it alone.
	case slots > 1:
		fields := make(multipleObjects, slots)
		s.fields = &fields
	default:
		// Violates precondition.
		panic(fmt.Sprintf("Alloc called with negative slots %d?", slots))
	}
}

// Fields returns the number of fields.
func (s *structValue) Fields() int {
	switch x := s.fields.(type) {
	case *multipleObjects:
		return len(*x)
	case noObjects:
		return 0
	default:
		return 1
	}
}

// loadStruct loads an object of type structValue.
func loadStruct(r *reader) structValue {
	return structValue{
		TypeID: typeSpecID(loadUint(r)),
		fields: loadObject(r),
	}
}

// save implements object.save.
//
// Precondition: Alloc must have been called, and the fields all filled in
// appropriately. See Alloc and Add for more details.
func (s *structValue) save(w *writer) {
	uintValue(s.TypeID).save(w)
	saveObject(w, s.fields)
}

// load implements object.load.
func (*structValue) load(r *reader) object {
	s := loadStruct(r)
	return &s
}

// object types.
//
// N.B. Be careful about changing the order or introducing new elements in the
// middle here. This is part of the wire format and shouldn't change.
const (
	typeBool uintValue = iota
	typeInt
	typeUint
	typeFloat32
	typeFloat64
	typeNil
	typeRef
	typeString
	typeSlice
	typeArray
	typeMap
	typeStruct
	typeNoObjects
	typeMultipleObjects
	typeInterface
	typeComplex64
	typeComplex128
	typeType
	typeFunction
	typeReflectType
	typeReflectValue
	typeChannel
	typeChannelData
	typeRawArray
)

// saveObject saves the given object.
//
// +checkescape all
//
// N.B. This function will panic on error.
func saveObject(w *writer, obj object) {
	switch x := obj.(type) {
	case boolValue:
		typeBool.save(w)
		x.save(w)
	case intValue:
		typeInt.save(w)
		x.save(w)
	case uintValue:
		typeUint.save(w)
		x.save(w)
	case float32Value:
		typeFloat32.save(w)
		x.save(w)
	case float64Value:
		typeFloat64.save(w)
		x.save(w)
	case nilValue:
		typeNil.save(w)
		x.save(w)
	case *refValue:
		typeRef.save(w)
		x.save(w)
	case *stringValue:
		typeString.save(w)
		x.save(w)
	case *sliceValue:
		typeSlice.save(w)
		x.save(w)
	case *arrayValue:
		typeArray.save(w)
		x.save(w)
	case *rawArrayValue:
		typeRawArray.save(w)
		x.save(w)
	case *mapValue:
		typeMap.save(w)
		x.save(w)
	case *structValue:
		typeStruct.save(w)
		x.save(w)
	case noObjects:
		typeNoObjects.save(w)
		x.save(w)
	case *multipleObjects:
		typeMultipleObjects.save(w)
		x.save(w)
	case *interfaceValue:
		typeInterface.save(w)
		x.save(w)
	case *typeDescriptor:
		typeType.save(w)
		x.save(w)
	case *complex64Value:
		typeComplex64.save(w)
		x.save(w)
	case *complex128Value:
		typeComplex128.save(w)
		x.save(w)
	case *functionValue:
		typeFunction.save(w)
		x.save(w)
	case *reflectTypeValue:
		typeReflectType.save(w)
		x.save(w)
	case *reflectedValue:
		typeReflectValue.save(w)
		x.save(w)
	case *channelValue:
		typeChannel.save(w)
		x.save(w)
	case *channelData:
		typeChannelData.save(w)
		x.save(w)
	default:
		panic(fmt.Errorf("unknown type: %#v", obj))
	}
}

// loadObject loads a new object.
//
// +checkescape all
//
// N.B. This function will panic on error.
func loadObject(r *reader) object {
	switch hdr := loadUint(r); hdr {
	case typeBool:
		return loadBool(r)
	case typeInt:
		return loadInt(r)
	case typeUint:
		return loadUint(r)
	case typeFloat32:
		return loadFloat32(r)
	case typeFloat64:
		return loadFloat64(r)
	case typeNil:
		return loadNil(r)
	case typeRef:
		return ((*refValue)(nil)).load(r) // Escapes.
	case typeString:
		return ((*stringValue)(nil)).load(r) // Escapes.
	case typeSlice:
		return ((*sliceValue)(nil)).load(r) // Escapes.
	case typeArray:
		return ((*arrayValue)(nil)).load(r) // Escapes.
	case typeRawArray:
		return ((*rawArrayValue)(nil)).load(r)
	case typeMap:
		return ((*mapValue)(nil)).load(r) // Escapes.
	case typeStruct:
		return ((*structValue)(nil)).load(r) // Escapes.
	case typeNoObjects: // Special for struct.
		return loadNoObjects(r)
	case typeMultipleObjects: // Special for struct.
		return ((*multipleObjects)(nil)).load(r) // Escapes.
	case typeInterface:
		return ((*interfaceValue)(nil)).load(r) // Escapes.
	case typeComplex64:
		return ((*complex64Value)(nil)).load(r) // Escapes.
	case typeComplex128:
		return ((*complex128Value)(nil)).load(r) // Escapes.
	case typeType:
		return ((*typeDescriptor)(nil)).load(r) // Escapes.
	case typeFunction:
		return ((*functionValue)(nil)).load(r)
	case typeReflectType:
		return ((*reflectTypeValue)(nil)).load(r)
	case typeReflectValue:
		return ((*reflectedValue)(nil)).load(r)
	case typeChannel:
		return ((*channelValue)(nil)).load(r)
	case typeChannelData:
		return ((*channelData)(nil)).load(r)
	default:
		// This is not a valid stream?
		panic(fmt.Errorf("unknown header: %d", hdr))
	}
}
