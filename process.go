package sandbox

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ProcessOptions configures the filesystem, environment and syscall observer
// of a Process. Configuration is fixed when the process first starts.
type ProcessOptions struct {
	Mounts  []Mount
	Env     []string
	Inspect func(*Syscall)
}

// Process executes successive callbacks in one guest. Captured objects are
// written back after each successful Run; native package globals remain in the
// guest. Idle guests are paused, and their private pages may be reclaimed after
// ten seconds. Close releases the process and its retained object identities.
// Do not copy a Process. Use NewProcess to create one.
type Process struct {
	options ProcessOptions
	owner   *Sandbox
	id      uint64

	runMu     sync.Mutex
	mu        sync.Mutex
	runs      sync.WaitGroup
	closeOnce sync.Once
	closed    bool
	failure   error
	closeErr  error
	native    processState
}

var nextProcessID atomic.Uint64

// NewProcess creates a process using the shared default Sandbox. It accepts
// zero or one options value. Startup is lazy; the first Run reports startup or
// configuration errors. A nil Env inherits the host environment at startup.
func NewProcess(opts ...ProcessOptions) *Process {
	p := &Process{owner: &defaultSandbox, id: nextProcessID.Add(1)}
	if len(opts) > 1 {
		p.failure = errors.New("sandbox: NewProcess accepts at most one options value")
		return p
	}
	if len(opts) == 1 {
		p.options = opts[0]
		if opts[0].Env != nil {
			p.options.Env = append([]string{}, opts[0].Env...)
		}
		p.options.Mounts = append([]Mount(nil), opts[0].Mounts...)
		for i := range p.options.Mounts {
			p.options.Mounts[i].Options = append([]string(nil), opts[0].Mounts[i].Options...)
		}
	}
	return p
}
