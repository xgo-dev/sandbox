//go:build linux && (amd64 || arm64) && cgo

package main

import (
	"fmt"
	"mime"
	"os"
	"sync/atomic"
	"time"

	"github.com/xgo-dev/sandbox"
	"golang.org/x/sys/unix"
)

var residentNumber int
var residentPointer *int
var residentBytes []byte
var residentStop chan struct{}

func checkProcessLifecycle() error {
	const heartbeat = 0xffffffd0
	var ticks atomic.Int64
	p := sandbox.NewProcess(sandbox.ProcessOptions{Inspect: func(call *sandbox.Syscall) {
		if call.Number == heartbeat {
			ticks.Add(1)
			call.Number = unix.SYS_GETPID
		}
	}})
	defer p.Close()
	x := 40
	if err := p.Run(func() {
		residentNumber = 7
		residentPointer = &x
		x++
		residentBytes = make([]byte, 64<<20)
		for i := range residentBytes {
			residentBytes[i] = byte(i*17 + 29)
		}
		residentStop = make(chan struct{})
		if err := mime.AddExtensionType(".sandbox-resident", "application/x-sandbox-resident"); err != nil {
			panic(err)
		}
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-residentStop:
					return
				case <-ticker.C:
					unix.RawSyscall(heartbeat, 0, 0, 0)
				}
			}
		}()
		// Publish one observation before the callback completes.
		unix.RawSyscall(heartbeat, 0, 0, 0)
	}); err != nil {
		return fmt.Errorf("persistent initial run: %w", err)
	}
	if x != 41 || residentNumber != 0 || residentPointer != nil || residentBytes != nil {
		return fmt.Errorf("persistent first writeback/global isolation: x=%d global=%d", x, residentNumber)
	}
	paused := ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if ticks.Load() != paused {
		return fmt.Errorf("guest kept executing after Run returned")
	}
	x = 50
	if err := p.Run(func() {
		check(residentNumber == 7 && residentPointer == &x && *residentPointer == 50, "resident global alias")
		check(mime.TypeByExtension(".sandbox-resident") == "application/x-sandbox-resident", "native MIME state")
		x++
		residentNumber++
	}); err != nil {
		return fmt.Errorf("persistent second run: %w", err)
	}
	if x != 51 {
		return fmt.Errorf("persistent second writeback: %d", x)
	}
	q := sandbox.NewProcess()
	defer q.Close()
	independent := false
	if err := q.Run(func() { independent = residentNumber == 0 && residentBytes == nil && residentPointer == nil }); err != nil {
		return err
	}
	if !independent {
		return fmt.Errorf("two processes shared native globals")
	}
	paused = ticks.Load()
	// Exercise the actual ten-second idle policy, then fault guest pages back.
	time.Sleep(11 * time.Second)
	if ticks.Load() != paused {
		return fmt.Errorf("idle eviction resumed the guest")
	}
	if err := q.Run(func() { check(residentNumber == 0, "other process changed during eviction") }); err != nil {
		return err
	}
	if err := p.Run(func() {
		check(residentNumber == 8 && residentPointer == &x, "resident state after eviction")
		for i, v := range residentBytes {
			check(v == byte(i*17+29), "resident bytes after eviction")
		}
		check(mime.TypeByExtension(".sandbox-resident") == "application/x-sandbox-resident", "native MIME after eviction")
		close(residentStop)
		x++
	}); err != nil {
		return fmt.Errorf("persistent run after idle: %w", err)
	}
	if x != 52 {
		return fmt.Errorf("persistent idle writeback: %d", x)
	}
	if err := p.Close(); err != nil {
		return err
	}
	if err := p.Close(); err != nil {
		return err
	}
	if err := p.Run(staticCall); err == nil {
		return fmt.Errorf("closed process accepted Run")
	}
	if err := q.Run(staticCall); err != nil {
		return fmt.Errorf("closing one process affected another: %w", err)
	}

	// Concurrent submissions to the same process must execute serially.
	serial := sandbox.NewProcess()
	defer serial.Close()
	results := make(chan error, 2)
	first, second := 0, 0
	go func() { results <- serial.Run(func() { residentNumber++; first = residentNumber }) }()
	go func() { results <- serial.Run(func() { residentNumber++; second = residentNumber }) }()
	for range 2 {
		if err := <-results; err != nil {
			return err
		}
	}
	if first+second != 3 || first == second {
		return fmt.Errorf("same-process calls overlapped or restarted: %d %d", first, second)
	}

	const waiting = 0xffffffd1
	ready := make(chan struct{})
	active := sandbox.NewProcess(sandbox.ProcessOptions{Inspect: func(call *sandbox.Syscall) {
		if call.Number == waiting {
			close(ready)
			call.Number = unix.SYS_GETPID
		}
	}})
	defer active.Close()
	original := x
	go func() {
		results <- active.Run(func() {
			x = 999
			unix.RawSyscall(waiting, 0, 0, 0)
			for {
				time.Sleep(time.Second)
			}
		})
	}()
	select {
	case <-ready:
	case err := <-results:
		return fmt.Errorf("active process failed before Close: %v", err)
	case <-time.After(20 * time.Second):
		return fmt.Errorf("active process did not start")
	}
	if err := active.Close(); err != nil {
		return err
	}
	if err := <-results; err == nil {
		return fmt.Errorf("terminated process Run succeeded")
	}
	if x != original {
		return fmt.Errorf("terminated process committed captures")
	}
	if err := q.Run(staticCall); err != nil {
		return err
	}

	// Inherited environment is captured at process startup, while explicit empty
	// environment remains empty. This process's init sees the supplied value.
	configured := sandbox.NewProcess(sandbox.ProcessOptions{Env: []string{"SANDBOX_ENV_TEST=resident"}})
	defer configured.Close()
	env := ""
	if err := configured.Run(func() { env = environmentAtInit; _ = os.Setenv("SANDBOX_ENV_TEST", "changed") }); err != nil {
		return err
	}
	if env != "resident" {
		return fmt.Errorf("process startup environment: %q", env)
	}
	if err := configured.Run(func() { env = os.Getenv("SANDBOX_ENV_TEST") }); err != nil {
		return err
	}
	if env != "changed" {
		return fmt.Errorf("process environment did not persist: %q", env)
	}
	fmt.Println("PASS persistent globals, captured aliases, pause, idle recovery, process isolation, serialized Run, environment persistence and Close")
	return nil
}
