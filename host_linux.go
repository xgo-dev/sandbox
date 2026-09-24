//go:build linux && (arm64 || amd64) && cgo

package sandbox

/*
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/cgo"
	"strings"
	"sync"
	"unsafe"

	"github.com/xgo-dev/sandbox/internal/state"
	"golang.org/x/sys/unix"
)

var libraryMu sync.Mutex

type inspection struct {
	mu  sync.Mutex
	fn  func(*Syscall)
	err error
}

//export sandboxInspect
func sandboxInspect(owner C.uintptr_t, event *C.struct_syscall_event) {
	i := cgo.Handle(owner).Value().(*inspection)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.fn == nil || i.err != nil {
		return
	}
	v := Syscall{Number: uint64(event.number), Name: C.GoString(event.name)}
	for n := range v.Args {
		v.Args[n] = uint64(event.args[n])
	}
	a := &syscallAccess{active: true}
	v.access = a
	mapped := false
	defer func() {
		if value := recover(); value != nil {
			i.err = fmt.Errorf("inspector panicked: %v", value)
			if mapped {
				event.failure = C.CString(i.err.Error())
			}
		}
		a.mu.Lock()
		a.active = false
		a.mmap = nil
		a.mu.Unlock()
	}()
	a.mmap = func(address uint64, size int) (Memory, error) {
		var memory C.struct_syscall_memory
		message := C.inspect_mmap(event, C.uint64_t(address), C.size_t(size), &memory)
		view := Memory{Data: unsafe.Slice((*byte)(memory.data), int(memory.length)), Addr: uint64(memory.address)}
		mapped = mapped || len(view.Data) != 0
		if message == nil {
			return view, nil
		}
		defer C.free(unsafe.Pointer(message))
		return view, fmt.Errorf("sandbox inspection: %s", C.GoString(message))
	}
	i.fn(&v)
	event.number = C.uint64_t(v.Number)
	for n, arg := range v.Args {
		event.args[n] = C.uint64_t(arg)
	}
}

type processState struct {
	kernel     uintptr
	inspection cgo.Handle
	observer   *inspection
	graph      state.State
	fn         func()
}

// Run executes fn in a new guest and closes it after writing captures back.
func (s *Sandbox) Run(fn func()) (err error) {
	p := NewProcess(ProcessOptions{Mounts: s.Mounts, Env: s.Env, Inspect: s.Inspect})
	p.owner = s
	defer func() { err = errors.Join(err, p.Close()) }()
	return p.Run(fn)
}

// Run executes fn in this process and pauses the guest before returning.
// Calls on one Process are serialized. Captures must be exclusively owned
// until Run returns. A failed transfer or guest execution closes the process.
func (p *Process) Run(fn func()) (err error) {
	if fn == nil {
		return errors.New("sandbox: nil function")
	}
	p.runMu.Lock()
	p.mu.Lock()
	if p.closed || p.failure != nil || p.owner == nil {
		err = p.failure
		if err == nil {
			err = errors.New("sandbox: process is closed or uninitialized")
		}
		p.mu.Unlock()
		p.runMu.Unlock()
		return err
	}
	p.runs.Add(1)
	p.mu.Unlock()
	transfer := false
	defer func() {
		if err != nil && transfer {
			p.mu.Lock()
			p.failure = err
			p.mu.Unlock()
		}
		p.runs.Done()
		p.runMu.Unlock()
		if err != nil && transfer {
			err = errors.Join(err, p.Close())
		}
	}()
	kernelID, err := p.owner.acquire()
	if err != nil {
		return err
	}
	defer p.owner.active.Done()
	fd, err := newStateImage()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	p.native.fn = fn
	resultOffset, err := writeStateImage(fd, 0, &p.native.graph, &p.native.fn)
	if err != nil {
		return fmt.Errorf("sandbox export: %w", err)
	}
	transfer = true
	runtime.GC()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("sandbox: process is closed")
	}
	if p.native.kernel == 0 {
		p.native.kernel = kernelID
		p.native.observer = &inspection{fn: p.options.Inspect}
		if p.options.Inspect != nil {
			p.native.inspection = cgo.NewHandle(p.native.observer)
		}
		err = p.call("create", fd)
		if err == nil {
			p.owner.mu.Lock()
			if p.owner.processes == nil {
				p.owner.processes = make(map[uint64]*Process)
			}
			p.owner.processes[p.id] = p
			p.owner.mu.Unlock()
		}
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if err := p.call("run", fd); err != nil {
		return err
	}
	p.native.observer.mu.Lock()
	inspectionErr := p.native.observer.err
	p.native.observer.mu.Unlock()
	if inspectionErr != nil {
		return inspectionErr
	}
	// Each call gets a new memfd. The previous result stays sealed even if the
	// resident guest retained a duplicate descriptor across calls.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_SEAL); err != nil {
		return fmt.Errorf("sandbox result seal: %w", err)
	}
	data, unmap, err := readStateImage(fd, resultOffset)
	if err != nil {
		return fmt.Errorf("sandbox result: %w", err)
	}
	defer func() { err = errors.Join(err, unmap()) }()
	if _, err := p.native.graph.Load(context.Background(), data, &p.native.fn); err != nil {
		return fmt.Errorf("sandbox import: %w", err)
	}
	runtime.GC()
	return nil
}

func (p *Process) call(operation string, fd int) error {
	config := struct {
		Operation string   `json:"operation"`
		Process   uint64   `json:"process"`
		Guest     string   `json:"guest,omitempty"`
		Mounts    []Mount  `json:"mounts,omitempty"`
		Env       []string `json:"env,omitempty"`
	}{Operation: operation, Process: p.id}
	var mainPC, entryPC uintptr
	if operation == "create" {
		var err error
		mainPC, err = guestMain()
		if err != nil {
			return err
		}
		entryPC = reflect.ValueOf(guestEntry).Pointer()
		config.Guest, err = os.Executable()
		if err != nil {
			return err
		}
		config.Mounts = p.options.Mounts
		if len(config.Mounts) == 0 {
			config.Mounts = []Mount{{Type: "bind", Source: "/", Target: "/", Options: []string{"ro"}}, {Type: "proc", Target: "/proc"}}
		}
		config.Env = p.options.Env
		if config.Env == nil {
			config.Env = os.Environ()
		}
	}
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("sandbox configuration: %w", err)
	}
	cConfig := C.CString(string(data))
	defer C.free(unsafe.Pointer(cConfig))
	var message [4096]C.char
	if C.sandbox_run(C.uintptr_t(p.native.kernel), cConfig, C.int(fd), C.uintptr_t(mainPC), C.uintptr_t(entryPC), C.uintptr_t(p.native.inspection), &message[0], C.size_t(len(message))) != 0 {
		return fmt.Errorf("sandbox Sentry: %s", C.GoString(&message[0]))
	}
	return nil
}

// Close terminates this process, including an active Run, and releases its
// resources. It is idempotent. Do not call it from this process's inspector.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		started := p.native.kernel != 0
		p.mu.Unlock()
		if started {
			p.closeErr = p.call("close", -1)
		}
		p.runs.Wait()
		if p.native.inspection != 0 {
			p.native.inspection.Delete()
			p.native.inspection = 0
		}
		p.native.graph = state.State{}
		p.native.fn = nil
		if p.owner != nil {
			p.owner.mu.Lock()
			delete(p.owner.processes, p.id)
			p.owner.mu.Unlock()
		}
	})
	return p.closeErr
}

func (s *Sandbox) acquire() (uintptr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, fmt.Errorf("sandbox: closed")
	}
	if s.kernel == 0 {
		if !validRseqSetting(os.Getenv("GLIBC_TUNABLES")) {
			return 0, fmt.Errorf("sandbox: start the process with GLIBC_TUNABLES=glibc.pthread.rseq=0")
		}
		library := s.Library
		if library == "" {
			executable, err := os.Executable()
			if err != nil {
				return 0, err
			}
			library = filepath.Join(filepath.Dir(executable), "sentrylib.so")
		}
		library, err := filepath.EvalSymlinks(library)
		if err != nil {
			return 0, err
		}
		library, err = filepath.Abs(library)
		if err != nil {
			return 0, err
		}
		cLibrary := C.CString(library)
		defer C.free(unsafe.Pointer(cLibrary))
		var handle C.uintptr_t
		var message [4096]C.char
		libraryMu.Lock()
		code := C.sandbox_create(cLibrary, &handle, &message[0], C.size_t(len(message)))
		libraryMu.Unlock()
		if code != 0 {
			return 0, fmt.Errorf("sandbox Sentry: %s", C.GoString(&message[0]))
		}
		s.kernel = uintptr(handle)
	}
	s.active.Add(1)
	return s.kernel, nil
}

// Close rejects new calls, terminates active guests, and waits for their Run
// calls and resources to finish. It is idempotent. Do not call Close from an
// inspector belonging to this Sandbox: Close waits for that callback to return.
func (s *Sandbox) Close() error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return s.closeErr
	}
	s.closed = true
	s.closeDone = make(chan struct{})
	handle := s.kernel
	s.mu.Unlock()
	var err error
	if handle != 0 {
		var message [4096]C.char
		if C.sandbox_close(C.uintptr_t(handle), &message[0], C.size_t(len(message))) != 0 {
			err = fmt.Errorf("sandbox Sentry: %s", C.GoString(&message[0]))
		}
	}
	s.active.Wait()
	s.mu.Lock()
	processes := make([]*Process, 0, len(s.processes))
	for _, p := range s.processes {
		processes = append(processes, p)
	}
	s.mu.Unlock()
	for _, p := range processes {
		err = errors.Join(err, p.Close())
	}
	s.closeErr = err
	close(s.closeDone)
	return err
}

func validRseqSetting(value string) bool {
	for _, setting := range strings.Split(value, ":") {
		if strings.HasPrefix(setting, "glibc.pthread.rseq=") {
			return setting == "glibc.pthread.rseq=0"
		}
	}
	return false
}
