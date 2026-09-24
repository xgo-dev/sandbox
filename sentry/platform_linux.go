//go:build linux && (arm64 || amd64)

package main

import (
	"sync"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

type inspector func(context.Context, *arch.Context64) error

type observedPlatform struct {
	platform.Platform
	mu        sync.Mutex
	processes map[string]*guestProcess
}

func (p *observedPlatform) NewContext(ctx context.Context) platform.Context {
	task := kernel.TaskFromContext(ctx)
	p.mu.Lock()
	process := p.processes[task.ContainerID()]
	process.tasks.Add(1)
	p.mu.Unlock()
	c := &observedContext{Context: p.Platform.NewContext(ctx), process: process, task: task}
	// NewTask holds its signal lock here and has not assigned task.p yet.
	// Only register the context; pause requests its stop outside taskMu.
	process.taskMu.Lock()
	process.contexts[c] = false
	process.taskMu.Unlock()
	return c
}

type observedContext struct {
	platform.Context
	process *guestProcess
	task    *kernel.Task
	mu      sync.Mutex // Serializes stop requests with platform context release.
}

func (c *observedContext) Release() {
	c.mu.Lock()
	p := c.process
	p.taskMu.Lock()
	stopped := p.contexts[c]
	p.taskMu.Unlock()
	// A task can finish its exit path without visiting doStop again. Balance
	// any request that raced with exit before releasing the platform context.
	if stopped {
		c.task.EndExternalStop()
	}
	c.Context.Release()
	p.taskMu.Lock()
	delete(p.contexts, c)
	p.taskMu.Unlock()
	c.mu.Unlock()
	p.tasks.Done()
}

func (c *observedContext) Switch(ctx context.Context, mm platform.MemoryManager, ac *arch.Context64, cpu int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	info, access, err := c.Context.Switch(ctx, mm, ac, cpu)
	if err == nil {
		ac.SyscallSaveOrig()
		if err := c.process.inspect(ctx, ac); err != nil {
			return nil, hostarch.NoAccess, err
		}
	}
	return info, access, err
}
