package reflectxtype

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"unsafe"
)

func TestMissingMethodSignatures(t *testing.T) {
	// StructOf caches the nil signature of a stripped embedded method. Keep
	// that fixture out of other tests which enumerate the process's caches.
	if os.Getenv("SANDBOX_MISSING_METHOD_SIGNATURES") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestMissingMethodSignatures$", "-test.v")
		command.Env = append(os.Environ(), "SANDBOX_MISSING_METHOD_SIGNATURES=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("missing method signatures: %v\n%s", err, output)
		}
		return
	}

	pointer := reflect.TypeFor[*bytes.Buffer]()
	raw := runtimeMethods((*[2]unsafe.Pointer)(unsafe.Pointer(&pointer))[1])
	var stripped int
	for _, method := range raw {
		if method.signature == -1 {
			stripped++
		}
	}
	if stripped == 0 {
		t.Fatal("bytes.Buffer fixture has no linker-stripped method signatures")
	}
	methods, _, _, _ := concreteMethodSet(pointer.Elem())
	if len(methods) != len(raw)-stripped {
		t.Fatalf("static methods: got %d, want %d", len(methods), len(raw)-stripped)
	}

	embedded := reflect.StructOf([]reflect.StructField{{Name: "Buffer", Type: pointer, Anonymous: true}})
	raw = runtimeMethods((*[2]unsafe.Pointer)(unsafe.Pointer(&embedded))[1])
	var missing int
	for _, method := range raw {
		if method.signature >= -1 {
			continue
		}
		reflectOffsetsLock()
		ptr, found := reflectOffsets.m[method.signature]
		reflectOffsetsUnlock()
		if found && ptr == nil {
			missing++
		}
	}
	if missing != stripped {
		t.Fatalf("StructOf nil signatures: got %d, want %d", missing, stripped)
	}
	methods, functions, _, entries := concreteMethodSet(embedded)
	if len(methods) != len(raw)-missing || len(functions) != len(methods) || len(entries) != len(methods) {
		t.Fatalf("dynamic method tables: methods=%d functions=%d entries=%d, want %d", len(methods), len(functions), len(entries), len(raw)-missing)
	}
	for i, method := range methods {
		if method.Type == nil || !functions[i].IsValid() {
			t.Fatalf("missing signature was exported: %s", method.Name)
		}
		if method.Name == "String" {
			receiver := reflect.New(embedded).Elem()
			receiver.Field(0).Set(reflect.ValueOf(bytes.NewBufferString("retained method")))
			if got := functions[i].Call([]reflect.Value{receiver})[0].String(); got != "retained method" {
				t.Fatalf("valid method returned %q", got)
			}
			return
		}
	}
	t.Fatal("valid String method was omitted")
}
