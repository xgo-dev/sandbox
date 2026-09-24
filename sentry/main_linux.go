//go:build linux && (arm64 || amd64) && cgo

package main

/*
#include <stdlib.h>
#include "sandbox.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"runtime/cgo"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/pkg/abi/linux"
	gcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
)

var kernels = struct {
	sync.Mutex
	next uintptr
	live map[uintptr]*sentryKernel
}{live: make(map[uintptr]*sentryKernel)}

func reportError(err error, message *C.char, capacity C.size_t) C.int {
	if err == nil {
		return 0
	}
	if capacity > 0 {
		buf := unsafe.Slice((*byte)(unsafe.Pointer(message)), int(capacity))
		n := copy(buf[:len(buf)-1], err.Error())
		buf[n] = 0
	}
	return 1
}

//export CreateSandbox
func CreateSandbox(handle *C.uintptr_t, message *C.char, capacity C.size_t) C.int {
	kernels.Lock()
	defer kernels.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	log.SetLevel(log.Warning)
	installSyscallMemory()
	p := &observedPlatform{processes: make(map[string]*guestProcess)}
	k, err := newKernel(p)
	if err != nil {
		return reportError(err, message, capacity)
	}
	if err := k.Start(); err != nil {
		k.Release()
		return reportError(err, message, capacity)
	}
	dog := watchdog.New(k, watchdog.DefaultOpts)
	dog.Start()
	kernels.next++
	kernels.live[kernels.next] = &sentryKernel{kernel: k, platform: p, dog: dog}
	*handle = C.uintptr_t(kernels.next)
	return 0
}

//export CloseSandbox
func CloseSandbox(handle C.uintptr_t, message *C.char, capacity C.size_t) C.int {
	kernels.Lock()
	s := kernels.live[uintptr(handle)]
	delete(kernels.live, uintptr(handle))
	kernels.Unlock()
	if s == nil {
		return 0
	}
	s.mu.Lock()
	s.closed = true
	processes := make([]*guestProcess, 0, len(s.processes))
	for _, p := range s.processes {
		processes = append(processes, p)
	}
	s.mu.Unlock()
	var closeErr error
	for _, p := range processes {
		closeErr = errors.Join(closeErr, p.close())
	}
	s.runs.Wait()
	s.kernel.Kill(linux.WaitStatusExit(1))
	s.kernel.WaitExited()
	s.dog.Stop()
	s.kernel.Release()
	return reportError(closeErr, message, capacity)
}

//export RunSandbox
func RunSandbox(handle C.uintptr_t, config *C.char, imageFD C.int, mainPC, entryPC, owner C.uintptr_t, callback C.inspect_fn, message *C.char, capacity C.size_t) (code C.int) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	report := func(err error) { code = reportError(err, message, capacity) }
	defer func() {
		if v := recover(); v != nil {
			report(fmt.Errorf("Sentry process operation panicked: %v", v))
		}
	}()
	var startup struct {
		Operation string   `json:"operation"`
		Process   uint64   `json:"process"`
		Guest     string   `json:"guest"`
		Mounts    []mount  `json:"mounts"`
		Env       []string `json:"env"`
	}
	if err := json.Unmarshal([]byte(C.GoString(config)), &startup); err != nil {
		report(fmt.Errorf("Sentry configuration: %w", err))
		return
	}
	kernels.Lock()
	s := kernels.live[uintptr(handle)]
	if s != nil {
		s.runs.Add(1)
	}
	kernels.Unlock()
	if s == nil {
		if startup.Operation != "close" {
			report(errors.New("sandbox is closed"))
		}
		return
	}
	defer s.runs.Done()
	if startup.Operation == "close" || startup.Operation == "run" {
		s.mu.Lock()
		process := s.processes[startup.Process]
		if startup.Operation == "close" {
			delete(s.processes, startup.Process)
		}
		closed := s.closed
		s.mu.Unlock()
		if startup.Operation == "close" {
			if process != nil {
				report(process.close())
			}
			return
		}
		if closed || process == nil {
			report(errors.New("process is closed or missing"))
			return
		}
		report(process.run(int(imageFD)))
		return
	}
	if startup.Operation != "create" {
		report(errors.New("invalid process operation"))
		return
	}
	process := &guestProcess{}
	process.inspect = func(ctx gcontext.Context, ac *arch.Context64) error {
		if callback == nil {
			return nil
		}
		task := kernel.TaskFromContext(ctx)
		m := &syscallMemory{ctx: task.Kernel().SupervisorContext(), mm: task.MemoryManager(), task: task}
		handle := cgo.NewHandle(m)
		defer handle.Delete()
		event := C.struct_syscall_event{number: C.uint64_t(ac.SyscallNo())}
		name := C.CString(task.SyscallTable().LookupName(ac.SyscallNo()))
		defer C.free(unsafe.Pointer(name))
		event.name = name
		C.prepare_inspection(&event, C.uintptr_t(handle))
		for i, arg := range ac.SyscallArgs() {
			event.args[i] = C.uint64_t(arg.Uint64())
		}
		C.invoke_inspector(callback, owner, &event)
		var err error
		if event.failure != nil {
			err = fmt.Errorf("syscall inspection: %s", C.GoString(event.failure))
			C.free(unsafe.Pointer(event.failure))
		}
		if len(m.regions) != 0 {
			if err != nil {
				err = errors.Join(err, m.release())
			} else {
				memoryCalls.Lock()
				memoryCalls.pending[task] = m
				memoryCalls.live[m] = struct{}{}
				memoryCalls.Unlock()
			}
		}
		if err != nil {
			process.inspectionMu.Lock()
			if process.inspectionErr == nil {
				process.inspectionErr = err
			}
			process.inspectionMu.Unlock()
			_ = task.Kernel().SendContainerSignal(task.ContainerID(), &linux.SignalInfo{Signo: int32(linux.SIGKILL)})
			return err
		}
		var args [6]uint64
		for i, arg := range event.args {
			args[i] = uint64(arg)
		}
		setSyscall(ac, uint64(event.number), args)
		ac.SyscallSaveOrig()
		return nil
	}
	report(s.start(startup.Process, process, startup.Mounts, startup.Guest, startup.Env, int(imageFD), uintptr(mainPC), uintptr(entryPC)))
	return
}

func main() {}
