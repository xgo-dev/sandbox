// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package main

// Adapted from runsc/boot/vfs.go, loader.go and loader_test.go at gVisor
// Go-export d1e35511e5a41ee5c2afc522d2f9b0c27cf8d382.

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/sentry/devices/memdev"
	"gvisor.dev/gvisor/pkg/sentry/fdimport"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/overlay"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/proc"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/runsc/fsgofer"
)

var procFDOnce sync.Once
var procFDErr error

type mount struct {
	Type    string   `json:"type"`
	Source  string   `json:"source,omitempty"`
	Target  string   `json:"target"`
	Options []string `json:"options,omitempty"`

	opts vfs.MountOptions
	ioFD *fd.FD
	stop func()
}

// Bind connections outlive the guest and its mount namespace. Release them
// last, including connections prepared before a later mount fails.
func closeMounts(mounts []mount) {
	for i := len(mounts) - 1; i >= 0; i-- {
		m := &mounts[i]
		if m.ioFD != nil {
			m.ioFD.Close()
		}
		if m.stop != nil {
			m.stop()
		}
	}
}

func prepareMounts(mounts []mount) error {
	if len(mounts) == 0 || mounts[0].Target != "/" || (mounts[0].Type != "bind" && mounts[0].Type != tmpfs.Name) {
		return fmt.Errorf("first mount must be bind or tmpfs at /")
	}
	targets := make(map[string]bool)
	for i := range mounts {
		m := &mounts[i]
		if !path.IsAbs(m.Target) || strings.ContainsRune(m.Target, 0) {
			return fmt.Errorf("mount target %q must be an absolute path without NUL", m.Target)
		}
		m.Target = path.Clean(m.Target)
		if targets[m.Target] {
			return fmt.Errorf("duplicate mount target %q", m.Target)
		}
		targets[m.Target] = true
		switch m.Type {
		case "bind":
			if !path.IsAbs(m.Source) || strings.ContainsRune(m.Source, 0) {
				return fmt.Errorf("bind source %q must be an absolute host path without NUL", m.Source)
			}
			// LISAFS opens the final component with O_NOFOLLOW. Resolve host
			// aliases such as /lib -> /usr/lib before donating the root FD.
			source, err := filepath.EvalSymlinks(m.Source)
			if err != nil {
				return fmt.Errorf("bind source: %w", err)
			}
			m.Source = source
			info, err := os.Stat(m.Source)
			if err != nil {
				return fmt.Errorf("bind source: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("bind source %q must be a directory", m.Source)
			}
		case tmpfs.Name, proc.Name, overlay.Name:
		default:
			return fmt.Errorf("unsupported filesystem type %q", m.Type)
		}
		m.opts.GetFilesystemOptions.InternalMount = true
		var data []string
		for _, option := range m.Options {
			if option == "" || strings.ContainsAny(option, ",\x00") {
				return fmt.Errorf("mount %q: invalid option %q", m.Target, option)
			}
			switch option {
			case "ro":
				m.opts.ReadOnly = true
			case "rw":
				m.opts.ReadOnly = false
			case "noexec":
				m.opts.Flags.NoExec = true
			case "exec":
				m.opts.Flags.NoExec = false
			case "nosuid":
				m.opts.Flags.NoSUID = true
			case "suid":
				m.opts.Flags.NoSUID = false
			case "noatime":
				m.opts.Flags.NoATime = true
			case "atime":
				m.opts.Flags.NoATime = false
			default:
				if m.Type != tmpfs.Name && m.Type != overlay.Name {
					return fmt.Errorf("mount %q: unsupported option %q", m.Target, option)
				}
				data = append(data, option)
			}
		}
		m.opts.GetFilesystemOptions.Data = strings.Join(data, ",")
	}
	for i := range mounts {
		m := &mounts[i]
		if m.Type == "bind" {
			var err error
			m.ioFD, m.stop, err = startFilesystem(m.Source, m.opts.ReadOnly)
			if err != nil {
				return fmt.Errorf("mount %q: %w", m.Target, err)
			}
		}
	}
	return nil
}

func startFilesystem(root string, readOnly bool) (*fd.FD, func(), error) {
	procFDOnce.Do(func() { procFDErr = fsgofer.OpenProcSelfFD("/proc/self/fd") })
	if procFDErr != nil {
		return nil, nil, procFDErr
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	client := fd.New(pair[0])
	socket, err := unet.NewSocket(pair[1])
	if err != nil {
		client.Close()
		unix.Close(pair[1])
		return nil, nil, err
	}
	server := lisafs.NewServer()
	impl := fsgofer.NewConnectionImpl(&fsgofer.Config{DonateMountPointFD: true})
	conn, err := server.CreateConnection(socket, root, fsgofer.ConnectionOpts(readOnly), impl)
	if err != nil {
		client.Close()
		socket.Close()
		server.Destroy()
		return nil, nil, err
	}
	server.StartConnection(conn)
	return client, func() {
		socket.Close()
		server.Wait()
		server.Destroy()
	}, nil
}

func registerFilesystems(k *kernel.Kernel) error {
	vfsObj := k.VFS()
	if err := memdev.Register(vfsObj); err != nil {
		return fmt.Errorf("registering memory devices: %w", err)
	}
	vfsObj.MustRegisterFilesystemType(gofer.Name, &gofer.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	vfsObj.MustRegisterFilesystemType(proc.Name, &proc.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	vfsObj.MustRegisterFilesystemType(tmpfs.Name, &tmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	vfsObj.MustRegisterFilesystemType(overlay.Name, &overlay.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	return nil
}

func mountFilesystem(ctx context.Context, k *kernel.Kernel, mounts []mount) (_ *vfs.MountNamespace, err error) {
	creds := auth.NewRootCredentials(k.RootUserNamespace())
	vfsObj := k.VFS()
	var mntns *vfs.MountNamespace
	var root vfs.VirtualDentry
	defer func() {
		if root.Ok() {
			root.DecRef(ctx)
		}
		if err != nil && mntns != nil {
			mntns.DecRef(ctx)
		}
	}()
	for i := range mounts {
		m := &mounts[i]
		if i != 0 {
			if err := vfsObj.MakeSyntheticMountpoint(ctx, m.Target, root, creds); err != nil {
				return nil, err
			}
		}
		fsType := m.Type
		if fsType == "bind" {
			fsType = gofer.Name
			ioFD := m.ioFD.Release()
			m.opts.GetFilesystemOptions.Data = fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,directfs,disable_fifo_open", ioFD, ioFD)
		}
		if i == 0 {
			mntns, err = vfsObj.NewMountNamespace(ctx, creds, m.Source, fsType, &m.opts, k)
			if err != nil {
				return nil, fmt.Errorf("mounting root: %w", err)
			}
			root = mntns.Root(ctx)
			ctx = vfs.WithRoot(vfs.WithMountNamespace(ctx, mntns), root)
			continue
		}
		if _, err := vfsObj.MountAt(ctx, creds, m.Source, &vfs.PathOperation{
			Root: root, Start: root, Path: fspath.Parse(m.Target), FollowFinalSymlink: true,
		}, fsType, &m.opts); err != nil {
			return nil, fmt.Errorf("mounting %q at %q: %w", fsType, m.Target, err)
		}
	}
	return mntns, nil
}

func importDescriptors(k *kernel.Kernel, imageFD, controlFD int) (*kernel.FDTable, error) {
	ctx := k.SupervisorContext()
	table := k.NewFDTable()
	files := make(map[int]*fd.FD)
	for n, descriptor := range []int{int(os.Stdin.Fd()), int(os.Stdout.Fd()), int(os.Stderr.Fd()), imageFD, controlFD} {
		dup, err := unix.Dup(descriptor)
		if err != nil {
			table.DecRef(ctx)
			return nil, err
		}
		files[n] = fd.New(dup)
		defer files[n].Close()
	}
	_, err := fdimport.Import(ctx, table, files, fdimport.ImportOptions{UID: 1000, GID: 1000, SupportTTYs: true})
	if err != nil {
		table.DecRef(ctx)
		return nil, err
	}
	for _, descriptor := range []int32{3, 4} {
		if err := table.SetFlags(ctx, descriptor, kernel.FDFlags{CloseOnExec: true}); err != nil {
			table.DecRef(ctx)
			return nil, err
		}
	}
	return table, nil
}
