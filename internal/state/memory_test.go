package state

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

func objectExamples() []object {
	s := stringValue("a\x00b\xff")
	c64, c128 := complex64Value(complex(1.5, -2.5)), complex128Value(complex(-3.5, 4.5))
	f := fieldName("Value")
	zero, one, many := &structValue{}, &structValue{TypeID: 1}, &structValue{TypeID: 2}
	zero.Alloc(0)
	one.Alloc(1)
	*one.Field(0) = intValue(42)
	many.Alloc(2)
	*many.Field(0), *many.Field(1) = boolValue(true), &s
	return []object{
		nilValue{}, boolValue(false), boolValue(true),
		intValue(math.MinInt64), intValue(math.MaxInt64), uintValue(math.MaxUint64),
		float32Value(math.Float32frombits(0x80000000)), float64Value(math.Inf(1)),
		float32Value(math.Float32frombits(0x7fc01234)), float64Value(math.Float64frombits(0x7ff8000000001234)),
		&c64, &c128, &s,
		&refValue{}, &refValue{Root: 2},
		&refValue{Root: 2, Type: typeSpecID(1)},
		&refValue{Root: 2, Dots: []dot{&f, index(3)}, Type: &arrayType{Count: 4, Type: typeSpecID(1)}},
		&refValue{Root: 2, Dots: []dot{index(1), arrayRange{start: 18, length: 6}, &f}},
		&sliceValue{Length: 2, Capacity: 3, Ref: refValue{Root: 2}},
		&arrayValue{}, &arrayValue{Contents: []object{intValue(1), intValue(2)}},
		&arrayValue{Contents: []object{nilValue{}, nilValue{}}},
		&rawArrayValue{}, &rawArrayValue{Data: []byte{0, 1, 127, 128, 255}},
		&mapValue{}, &mapValue{Keys: []object{intValue(1), intValue(2)}, Values: []object{boolValue(true), boolValue(false)}},
		zero, one, many,
		&interfaceValue{Type: nilType{}, Value: nilValue{}},
		&interfaceValue{Type: &pointerType{Type: &sliceType{Type: &mapType{Key: typeSpecID(1), Value: typeSpecID(2)}}}, Value: &refValue{Root: 3}},
		&typeDescriptor{Name: "test.Node", Fields: []string{"Value", "Next"}},
		&functionValue{}, &functionValue{PC: 0x520000, Env: refValue{Root: 2}},
		&functionValue{PC: 0x520000},
		&functionValue{PC: 0x520000, Env: refValue{Root: 2}, Type: &reflectedType{ID: 1}},
		&functionValue{PC: 0x520000, Env: refValue{Root: 2}, Type: &reflectedType{ID: 1, reflectx: true}},
		&channelValue{}, &channelValue{Capacity: 3, Ref: refValue{Root: 2}},
		&channelData{}, &channelData{Closed: true, Values: arrayValue{Contents: []object{intValue(10), intValue(20)}}},
		&reflectedValue{Type: nilType{}, Value: nilValue{}},
		&reflectTypeValue{Type: &reflectedType{ID: 1}},
		&reflectTypeValue{Type: &reflectedType{ID: 1, reflectx: true}},
		&reflectedValue{Type: typeSpecID(1), Value: intValue(42)},
		&reflectedValue{Type: &pointerType{Type: typeSpecID(1)}, Value: &refValue{Root: 2}, Addressable: true},
		&reflectedValue{Type: typeSpecID(1), Value: intValue(42), ReadOnly: 1 << 5},
		&reflectedValue{Type: typeSpecID(1), Value: intValue(42), ReadOnly: 1 << 6},
		&reflectedValue{Type: &pointerType{Type: typeSpecID(1)}, Value: &refValue{Root: 2}, Addressable: true, ReadOnly: reflectValueReadOnlyMask},
		&refValue{Root: 3, Dots: []dot{&f}, Type: closureType(0x520000)},
	}
}

func TestMemoryObjects(t *testing.T) {
	for _, obj := range objectExamples() {
		t.Run(reflect.TypeOf(obj).String(), func(t *testing.T) {
			backing := bytes.Repeat([]byte{0xa5}, 4098)
			w := writer{mem: backing[1 : len(backing)-1]}
			if err := w.put(obj); err != nil {
				t.Fatal(err)
			}
			if backing[0] != 0xa5 || backing[len(backing)-1] != 0xa5 || &w.mem[0] != &backing[1] {
				t.Fatal("writer changed its backing memory or wrote beyond it")
			}
			var stream bytes.Buffer
			streamed := writer{out: &stream}
			if err := streamed.put(obj); err != nil {
				t.Fatal(err)
			}
			if streamed.pos != w.pos || !bytes.Equal(stream.Bytes(), w.mem[:w.pos]) {
				t.Fatal("stream output differs from memory output")
			}
			r := reader{mem: w.mem[:w.pos]}
			got, err := r.get()
			if err != nil || r.pos != w.pos {
				t.Fatalf("get: %v, read=%d written=%d", err, r.pos, w.pos)
			}
			// Re-encoding also checks NaN payloads and signed zero, which
			// reflect.DeepEqual cannot distinguish correctly.
			again := writer{mem: make([]byte, w.pos)}
			if err := again.put(got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(w.mem[:w.pos], again.mem[:again.pos]) {
				t.Fatal("object bytes changed after decoding")
			}
		})
	}
}

func TestMemoryReflectValueReadOnlyFlags(t *testing.T) {
	for _, flags := range []uint64{1 << 7, 1 << 8, 1 << 63, ^uint64(0)} {
		w := writer{mem: make([]byte, 128)}
		if err := w.put(&reflectedValue{Type: typeSpecID(1), Value: intValue(42), ReadOnly: flags}); err != nil {
			t.Fatal(err)
		}
		r := reader{mem: w.mem[:w.pos]}
		if _, err := r.get(); err == nil || !strings.Contains(err.Error(), "invalid reflect.Value read-only flags") {
			t.Fatalf("accepted flags %#x: %v", flags, err)
		}
	}
}

func TestMemoryBoundaries(t *testing.T) {
	for _, obj := range objectExamples() {
		w := writer{mem: make([]byte, 4096)}
		if err := w.put(obj); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < w.pos; n++ {
			r := reader{mem: w.mem[:n]}
			if _, err := r.get(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("%T prefix %d/%d: %v", obj, n, w.pos, err)
			}
			guarded := bytes.Repeat([]byte{0xa5}, n+1)
			short := writer{mem: guarded[:n]}
			if err := short.put(obj); !errors.Is(err, io.ErrShortBuffer) {
				t.Fatalf("%T capacity %d/%d: %v", obj, n, w.pos, err)
			}
			if guarded[n] != 0xa5 {
				t.Fatal("writer crossed the provided slice length")
			}
		}
	}
}

func TestMemoryInvalidInput(t *testing.T) {
	for _, mem := range [][]byte{
		{0xff, 0x01}, // Unknown object tag.
		append([]byte{byte(typeUint)}, bytes.Repeat([]byte{0xff}, 10)...),
		binary.AppendUvarint([]byte{byte(typeString)}, math.MaxUint64),
		{byte(typeInterface), 0xff, 0x01}, // Unknown type tag.
	} {
		r := reader{mem: mem}
		if _, err := r.get(); err == nil {
			t.Fatalf("accepted %x", mem)
		}
	}
}

func TestDecodedStringOwnsItsBytes(t *testing.T) {
	s := stringValue("hello")
	w := writer{mem: make([]byte, 32)}
	if err := w.put(&s); err != nil {
		t.Fatal(err)
	}
	r := reader{mem: w.mem[:w.pos]}
	v, err := r.get()
	if err != nil {
		t.Fatal(err)
	}
	clear(w.mem)
	if *v.(*stringValue) != s {
		t.Fatal("decoded string aliases the encoded memory")
	}
}

func TestMemoryReferenceBytes(t *testing.T) {
	w := writer{mem: make([]byte, 32)}
	for _, obj := range []object{&arrayValue{Contents: []object{&refValue{Root: 2}, &refValue{Root: 2}}}, intValue(42)} {
		if err := w.put(obj); err != nil {
			t.Fatal(err)
		}
	}
	// An array of two references to object 2, followed by the signed integer
	// 42. Array elements share the first element's tag in the upstream format.
	want := []byte{9, 2, 6, 2, 0, 2, 0, 1, 84}
	if !bytes.Equal(w.mem[:w.pos], want) {
		t.Fatalf("got %x, want %x", w.mem[:w.pos], want)
	}
}
