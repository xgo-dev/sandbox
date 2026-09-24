//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/xgo-dev/sandbox"
	"golang.org/x/sys/unix"
)

type node struct {
	Value int
	Next  *node
}

var initializedPID = os.Getpid()
var enteredMain bool

func check(ok bool, message string) {
	if !ok {
		panic(message)
	}
}
func staticCall() { runtime.GC() }

func main() {
	enteredMain = true
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if err := checkProcessLifecycle(); err != nil {
		return fmt.Errorf("persistent process: %w", err)
	}
	if err := checkKernelLifecycle(); err != nil {
		return fmt.Errorf("kernel lifecycle: %w", err)
	}
	if err := checkEnvironment(); err != nil {
		return err
	}
	if err := checkErrorMessage(); err != nil {
		return err
	}
	var calls atomic.Int64
	s := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) { calls.Add(1) }}
	defer s.Close()
	n := 41
	pid := os.Getpid()
	if err := s.Run(func() {
		check(!enteredMain && initializedPID == os.Getpid(), "guest must run package init and skip main")
		n++
		runtime.GC()
	}); err != nil {
		return fmt.Errorf("integer: %w", err)
	}
	check(n == 42 && os.Getpid() == pid, "integer writeback/host identity")
	fmt.Println("PASS automatic guest entry after init, integer capture, host PID, GC, and Switch inspector")

	a := &node{Value: 1}
	a.Next = a
	alias := a
	m := map[string]*node{"a": a}
	mapAlias := m
	backing := []int{1, 2, 3}
	values := backing[:2]
	var boxed any = a
	var result complex128
	inc := func() { a.Value++ }
	if err := s.Run(func() {
		check(a == m["a"] && a.Next == a && boxed.(*node) == a, "guest aliases/cycle")
		inc()
		values[0] = 9
		values = append(values, 8, 7)
		delete(m, "a")
		m["b"] = a
		result = complex(3, 4)
		boxed = "finished"
	}); err != nil {
		return fmt.Errorf("objects: %w", err)
	}
	check(a == alias && a.Next == a && a.Value == 2, "host pointer/cycle")
	check(mapAlias["b"] == a && len(mapAlias) == 1, "host map identity")
	check(backing[0] == 9 && len(values) == 4 && values[3] == 7, "slice writeback/growth")
	check(boxed == "finished" && result == complex(3, 4), "interface/scalars")
	fmt.Println("PASS pointers, cycle, map aliases, slice growth, interface, nested closure")

	p := &node{Value: 10}
	old := p
	if err := s.Run(func() { p.Value = 11; p = nil }); err != nil {
		return fmt.Errorf("dropped reference: %w", err)
	}
	check(p == nil && old.Value == 11, "dropped object mutation lost")
	fmt.Println("PASS mutation before dropping last captured reference")

	before := n
	if err := s.Run(func() { n = 999; panic("expected smoke panic") }); err == nil {
		return fmt.Errorf("guest panic was not reported")
	}
	check(n == before, "guest panic committed a partial result")
	fmt.Println("PASS guest panic leaves host captures unchanged")
	if err := s.Run(staticCall); err != nil {
		return fmt.Errorf("static function: %w", err)
	}
	fmt.Println("PASS static function and repeated guest startup")
	for range 3 {
		if err := s.Run(staticCall); err != nil {
			return err
		}
	}
	fdBefore, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for range 3 {
		if err := s.Run(staticCall); err != nil {
			return err
		}
	}
	fdAfter, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	check(len(fdAfter) <= len(fdBefore)+1, fmt.Sprintf("per-call FD growth: %d -> %d", len(fdBefore), len(fdAfter)))
	fmt.Printf("PASS repeated-call descriptor count=%d\n", len(fdAfter))
	var guestPID uintptr
	var nestedErr error
	var rewriter sandbox.Sandbox
	rewriter.Inspect = func(call *sandbox.Syscall) {
		if call.Number == 0xfffffff0 {
			nestedErr = rewriter.Run(staticCall)
			call.Number = unix.SYS_GETPID
		}
	}
	defer rewriter.Close()
	if err := rewriter.Run(func() {
		result, _, errno := unix.RawSyscall(0xfffffff0, 0, 0, 0)
		if errno != 0 {
			panic(errno)
		}
		guestPID = result
	}); err != nil {
		return fmt.Errorf("syscall rewrite: %w", err)
	}
	check(guestPID == 1 && nestedErr == nil, fmt.Sprintf("syscall rewrite/nested Run: %v", nestedErr))
	fmt.Println("PASS syscall number rewrite and nested Run on the same Kernel")

	var mu sync.Mutex
	if err := s.Run(func() { mu.Lock(); mu.Unlock() }); err != nil {
		return fmt.Errorf("sync.Mutex reset: %w", err)
	}
	queue := &struct{ C chan int }{make(chan int, 2)}
	queue.C <- 7
	original := queue.C
	if err := s.Run(func() { value := <-queue.C; queue.C <- value + 1; close(queue.C) }); err != nil {
		return fmt.Errorf("channel snapshot: %w", err)
	}
	value, ok := <-queue.C
	check(ok && value == 8 && queue.C != original, "returned channel snapshot")
	_, ok = <-queue.C
	check(!ok && <-original == 7, "channel closed state/host isolation")
	fmt.Println("PASS reset synchronization values and independent channel snapshot")

	var stdinBefore, stdinAfter unix.Stat_t
	if err := unix.Fstat(0, &stdinBefore); err != nil {
		return err
	}
	var text string
	if err := s.Run(func() {
		b, err := os.ReadFile("/etc/hostname")
		if err != nil {
			panic(err)
		}
		text = string(b)
	}); err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	if err := unix.Fstat(0, &stdinAfter); err != nil {
		return err
	}
	check(stdinBefore.Ino == stdinAfter.Ino && stdinBefore.Dev == stdinAfter.Dev && text != "", "stdin changed/read missing")
	check(calls.Load() > 0, "no inspector callbacks")
	fmt.Printf("PASS read syscall, stdin preserved, inspector callbacks=%d\n", calls.Load())
	if err := inspectMemory(); err != nil {
		return err
	}
	if err := inspectTemporaryMemory(); err != nil {
		return err
	}
	if err := inspectIxgo(); err != nil {
		return err
	}
	return nil
}

func inspectMemory() error {
	dir, err := os.MkdirTemp("", "sandbox-inspection-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "input")
	if err := os.WriteFile(path, []byte("fixture content"), 0644); err != nil {
		return err
	}
	var retained *sandbox.Syscall
	var inspectionErr error
	pathRead, bufferRead := false, false
	s := sandbox.Sandbox{Inspect: func(call *sandbox.Syscall) {
		if inspectionErr != nil {
			return
		}
		switch call.Name {
		case "openat":
			view, err := call.MMap(call.Args[1], len(path)+1)
			if string(view.Data) != path+"\x00" {
				return
			}
			if err != nil {
				inspectionErr = fmt.Errorf("guest memory read: %d %v", len(view.Data), err)
				return
			}
			if _, err := call.MMap(1, len(path)+1); err == nil {
				inspectionErr = fmt.Errorf("unmapped guest memory read succeeded")
				return
			}
			pathRead = true
			retained = call
		case "write":
			if call.Args[2] != uint64(len("inspection payload")) {
				return
			}
			view, err := call.MMap(call.Args[1], len("inspection payload"))
			if err != nil {
				inspectionErr = err
				return
			}
			if string(view.Data) != "inspection payload" {
				return
			}
			bufferRead = true
			call.Args[2] = uint64(len("inspection"))
		}
	}}
	var content, received string
	var written int
	defer s.Close()
	err = s.Run(func() {
		data, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		content = string(data)
		var pipe [2]int
		if err := unix.Pipe(pipe[:]); err != nil {
			panic(err)
		}
		defer unix.Close(pipe[0])
		defer unix.Close(pipe[1])
		written, err = unix.Write(pipe[1], []byte("inspection payload"))
		if err != nil {
			panic(err)
		}
		var buf [128]byte
		n, err := unix.Read(pipe[0], buf[:])
		if err != nil {
			panic(err)
		}
		received = string(buf[:n])
	})
	if inspectionErr != nil {
		return fmt.Errorf("memory inspector: %w", inspectionErr)
	}
	if err != nil {
		return fmt.Errorf("guest memory inspection: %w", err)
	}
	check(pathRead && content == "fixture content", "openat pathname read")
	check(bufferRead && received == "inspection" && written == len(received), "write buffer read and raw count edit")
	if _, err := retained.MMap(1, 1); err == nil {
		return fmt.Errorf("retained MMap was accepted")
	}
	fmt.Println("PASS guest path/buffer reads, raw syscall argument edit, invalid address and callback lifetime")
	return nil
}
