// Package sandbox executes a Go closure in Sentry and copies changes to its
// captured object graph back to the caller. See README.md for transfer limits.
package sandbox

import (
	"errors"
	"sync"
)

// Syscall is a trapped guest syscall, observed after Context.Switch returns and
// before Sentry dispatches it. Addresses in Args belong to the guest.
type Syscall struct {
	Number uint64
	Args   [6]uint64
	Name   string
	access *syscallAccess
}

// Mount describes a filesystem in the guest namespace. Source is a host
// directory for bind mounts; overlay paths in Options refer to guest paths.
type Mount struct {
	Type    string   `json:"type"`
	Source  string   `json:"source,omitempty"`
	Target  string   `json:"target"`
	Options []string `json:"options,omitempty"`
}

// Sandbox owns a Kernel shared by its Run calls. The zero value is ready for
// use; the first Run starts the Kernel. Close releases it. Do not copy a Sandbox
// after first use or modify its configuration while a Run is active.
// Library defaults to sentrylib.so beside the calling executable and must not
// change after the first Run.
type Sandbox struct {
	Library string
	// An empty Mounts uses a read-only host root and guest procfs. Otherwise
	// the list replaces all defaults, starting with a bind or tmpfs at /.
	Mounts  []Mount
	Inspect func(*Syscall)
	// Env contains the guest's KEY=value environment entries. Nil inherits
	// the host environment at each Run; a non-nil slice replaces it entirely.
	// An empty non-nil slice starts the guest with no environment variables.
	Env []string

	mu        sync.Mutex
	kernel    uintptr
	active    sync.WaitGroup
	closed    bool
	closeDone chan struct{}
	closeErr  error
	processes map[uint64]*Process
}

var defaultSandbox Sandbox

// Run executes fn in a one-shot Process using the shared default Sandbox.
func Run(fn func()) (err error) {
	p := NewProcess()
	defer func() { err = errors.Join(err, p.Close()) }()
	return p.Run(fn)
}
