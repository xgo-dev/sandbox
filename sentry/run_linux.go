//go:build linux && (amd64 || arm64) && cgo

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/host"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/usermem"
)

type sentryKernel struct {
	mu        sync.Mutex
	kernel    *kernel.Kernel
	platform  *observedPlatform
	dog       *watchdog.Watchdog
	runs      sync.WaitGroup
	closed    bool
	processes map[uint64]*guestProcess
}

type guestProcess struct {
	inspect       inspector
	tasks         sync.WaitGroup
	taskMu        sync.Mutex // Protects registration and external-stop ownership.
	contexts      map[*observedContext]bool
	kernel        *kernel.Kernel
	id            string
	tg            *kernel.ThreadGroup
	control       *os.File
	mu            sync.Mutex // Serializes commands, pauses and idle eviction.
	paused        bool
	idleTimer     *time.Timer
	closeOnce     sync.Once
	stopping      chan struct{}
	exited        chan struct{}
	done          chan struct{}
	exitErr       error
	closeErr      error
	inspectionMu  sync.Mutex
	inspectionErr error
}

type mountContext struct{ context.Context }

func (c mountContext) Value(key any) any {
	if key == vfs.CtxMountNamespace {
		return nil
	}
	return c.Context.Value(key)
}

func (s *sentryKernel) start(processID uint64, p *guestProcess, mounts []mount, executable string, env []string, imageFD int, mainPC, entryPC uintptr) (err error) {
	if !path.IsAbs(executable) || strings.ContainsRune(executable, 0) {
		return errors.New("guest executable must be an absolute path without NUL")
	}
	for _, entry := range env {
		if strings.ContainsRune(entry, 0) {
			return errors.New("guest environment contains NUL")
		}
	}
	jump, err := entryJump(mainPC, entryPC)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			closeMounts(mounts)
		}
	}()
	if err := prepareMounts(mounts); err != nil {
		return err
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return err
	}
	p.control = os.NewFile(uintptr(pair[0]), "sandbox-control")
	defer unix.Close(pair[1])
	defer func() {
		if err != nil {
			p.control.Close()
		}
	}()
	// The guest sees a blocking descriptor. host.NewFD makes its host backing
	// nonblocking and implements guest blocking through Sentry's wait queues.
	if err := unix.SetNonblock(pair[1], false); err != nil {
		return err
	}
	k := s.kernel
	p.kernel, p.id = k, strconv.FormatUint(processID, 10)
	p.stopping, p.exited, p.done = make(chan struct{}), make(chan struct{}), make(chan struct{})
	p.contexts = make(map[*observedContext]bool)
	pidns := k.RootPIDNamespace().NewChild(k.SupervisorContext(), k, k.RootUserNamespace())
	defer pidns.DecRef(k.SupervisorContext())
	ls, err := limits.NewLinuxLimitSet()
	if err != nil {
		return err
	}
	args := kernel.CreateProcessArgs{Filename: executable, Argv: []string{executable}, Envv: env, WorkingDirectory: "/", Credentials: auth.NewUserCredentials(1000, 1000, nil, &auth.TaskCapabilities{}, k.RootUserNamespace()), Umask: 0022, Limits: ls, MaxSymlinkTraversals: linux.MaxSymlinkTraversals, UTSNamespace: k.RootUTSNamespace(), IPCNamespace: k.RootIPCNamespace(), PIDNamespace: pidns, ContainerID: p.id}
	ctx := mountContext{args.NewContext(k)}
	mntns, err := mountFilesystem(ctx, k, mounts)
	if err != nil {
		return err
	}
	defer mntns.DecRef(ctx)
	table, err := importDescriptors(k, imageFD, pair[1])
	if err != nil {
		return err
	}
	defer table.DecRef(ctx)
	args.FDTable, args.MountNamespace = table, mntns
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	if _, exists := s.processes[processID]; exists {
		s.mu.Unlock()
		return errors.New("process already exists")
	}
	s.platform.mu.Lock()
	s.platform.processes[p.id] = p
	s.platform.mu.Unlock()
	mntns.IncRef()
	tg, _, err := k.CreateProcess(args)
	if err != nil {
		s.platform.mu.Lock()
		delete(s.platform.processes, p.id)
		s.platform.mu.Unlock()
		s.mu.Unlock()
		return fmt.Errorf("loading guest: %w", err)
	}
	p.tg = tg
	_, err = tg.Leader().MemoryManager().CopyOut(ctx, hostarch.Addr(mainPC), jump, usermem.IOOpts{IgnorePermissions: true})
	if err != nil {
		_ = tg.SendSignal(&linux.SignalInfo{Signo: int32(linux.SIGKILL)})
	}
	k.StartProcess(tg)
	if err != nil {
		s.mu.Unlock()
		tg.WaitExited()
		p.tasks.Wait()
		s.platform.mu.Lock()
		delete(s.platform.processes, p.id)
		s.platform.mu.Unlock()
		return fmt.Errorf("installing guest entry: %w", err)
	}
	if s.processes == nil {
		s.processes = make(map[uint64]*guestProcess)
	}
	s.processes[processID] = p
	go func() {
		tg.WaitExited()
		p.tasks.Wait()
		p.control.Close()
		status := tg.ExitStatus()
		if !status.Exited() || status.ExitStatus() != 0 {
			p.exitErr = fmt.Errorf("application exited with %s", status)
		}
		close(p.exited)
		p.mu.Lock()
		if p.idleTimer != nil {
			p.idleTimer.Stop()
			p.idleTimer = nil
		}
		p.closeErr = releaseSyscallMemory(k, p.id)
		closeMounts(mounts)
		s.platform.mu.Lock()
		delete(s.platform.processes, p.id)
		s.platform.mu.Unlock()
		p.mu.Unlock()
		close(p.done)
	}()
	s.mu.Unlock()
	return nil
}

func (p *guestProcess) run(imageFD int) (runErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer func() {
		// An inspection failure terminates the guest and closes its control
		// socket. Preserve that cause instead of reporting the resulting EOF.
		p.inspectionMu.Lock()
		if p.inspectionErr != nil {
			runErr = p.inspectionErr
		}
		p.inspectionMu.Unlock()
	}()
	select {
	case <-p.stopping:
		return errors.New("process is closed")
	case <-p.exited:
		return errors.Join(errors.New("process has exited"), p.exitErr)
	default:
	}
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
	if p.paused {
		if err := p.installImage(imageFD); err != nil {
			return err
		}
		p.resume()
	}
	command := [1]byte{1}
	_, err := p.control.Write(command[:])
	if err == nil {
		_, err = io.ReadFull(p.control, command[:])
	}
	if err == nil && command[0] != 1 {
		err = errors.New("invalid guest completion message")
	}
	if err != nil {
		_ = p.kernel.SendContainerSignal(p.id, &linux.SignalInfo{Signo: int32(linux.SIGKILL)})
		<-p.exited
		return errors.Join(err, p.exitErr)
	}
	if err := p.pause(); err != nil {
		return err
	}
	p.inspectionMu.Lock()
	inspectionErr := p.inspectionErr
	p.inspectionMu.Unlock()
	if inspectionErr != nil {
		return inspectionErr
	}
	var timer *time.Timer
	timer = time.AfterFunc(10*time.Second, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.idleTimer != timer {
			return
		}
		p.idleTimer = nil
		select {
		case <-p.stopping:
			return
		case <-p.exited:
			return
		default:
		}
		if err := p.evict(); err != nil {
			log.Warningf("Idle page eviction for process %s failed: %v", p.id, err)
		}
	})
	p.idleTimer = timer
	return nil
}

func (p *guestProcess) installImage(imageFD int) error {
	ctx := p.kernel.SupervisorContext()
	duplicate, err := unix.Dup(imageFD)
	if err != nil {
		return err
	}
	file, err := host.NewFD(ctx, p.kernel.HostMount(), duplicate, &host.NewFDOptions{Savable: true, VirtualOwner: true, UID: 1000, GID: 1000})
	if err != nil {
		unix.Close(duplicate)
		return err
	}
	defer file.DecRef(ctx)
	var table *kernel.FDTable
	p.tg.Leader().WithMuLocked(func(task *kernel.Task) {
		table = task.FDTable()
		if table != nil {
			table.IncRef()
		}
	})
	if table == nil {
		return errors.New("process descriptor table has been released")
	}
	defer table.DecRef(ctx)
	old, err := table.NewFDAt(ctx, 3, file, kernel.FDFlags{CloseOnExec: true})
	if old != nil {
		old.DecRef(ctx)
	}
	return err
}

// pause is called with p.mu held. Newly registered contexts join the same stop
// round. For example, a clone already executing when pause starts may register
// its child after the first pass; its parent cannot acknowledge until it does.
func (p *guestProcess) pause() (err error) {
	defer func() {
		if err != nil {
			p.resume()
		}
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		p.taskMu.Lock()
		var pending []*observedContext
		for c, stopped := range p.contexts {
			if !stopped {
				pending = append(pending, c)
			}
		}
		p.taskMu.Unlock()
		for _, c := range pending {
			c.mu.Lock()
			p.taskMu.Lock()
			_, live := p.contexts[c]
			p.taskMu.Unlock()
			if live {
				// Never hold taskMu across gVisor calls: NewContext registers
				// while holding the signal lock that BeginExternalStop needs.
				c.task.BeginExternalStop()
				p.taskMu.Lock()
				p.contexts[c] = true
				p.taskMu.Unlock()
			}
			c.mu.Unlock()
		}
		p.taskMu.Lock()
		all := len(p.contexts) > 0
		for c, stopped := range p.contexts {
			if !stopped || c.task.TaskGoroutineState() != kernel.TaskGoroutineStopped {
				all = false
				break
			}
		}
		// Check membership and acknowledgements together. Once all tasks are
		// stopped, none can create another task until this process resumes.
		p.taskMu.Unlock()
		if all {
			p.paused = true
			return nil
		}
		select {
		case <-p.stopping:
			return errors.New("process is closing")
		case <-p.exited:
			return errors.Join(errors.New("process exited before pausing"), p.exitErr)
		case <-ticker.C:
		}
	}
}

// resume releases only this process's external stops. p.mu must be held, so
// pause cannot add requests while this pass releases them.
func (p *guestProcess) resume() {
	p.taskMu.Lock()
	var stopped []*observedContext
	for c, held := range p.contexts {
		if held {
			stopped = append(stopped, c)
		}
	}
	p.taskMu.Unlock()
	for _, c := range stopped {
		c.mu.Lock()
		p.taskMu.Lock()
		held := p.contexts[c]
		if held {
			p.contexts[c] = false
		}
		p.taskMu.Unlock()
		if held {
			c.task.EndExternalStop()
		}
		c.mu.Unlock()
	}
	p.paused = false
}

func (p *guestProcess) close() error {
	p.closeOnce.Do(func() {
		close(p.stopping)
		// Closing a pollable control socket wakes an active run. Take mu only
		// afterwards so Close can interrupt a callback that never returns.
		p.control.Close()
		p.mu.Lock()
		if p.idleTimer != nil {
			p.idleTimer.Stop()
			p.idleTimer = nil
		}
		_ = p.kernel.SendContainerSignal(p.id, &linux.SignalInfo{Signo: int32(linux.SIGKILL)})
		// SIGKILL does not release external stops. Queue it first, then let
		// stopped tasks run their exit paths before waiting for cleanup.
		p.resume()
		p.mu.Unlock()
		<-p.done
	})
	return p.closeErr
}
