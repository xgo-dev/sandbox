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

// Adapted from runsc/boot/loader.go and pkg/sentry/fsimpl/testutil/kernel.go at
// gVisor Go-export d1e35511e5a41ee5c2afc522d2f9b0c27cf8d382. Only startup for the
// embedded Kernel is retained; gVisor library sources are unchanged.

import (
	"fmt"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/cgroup2fs"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/host"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/loader"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	_ "gvisor.dev/gvisor/pkg/sentry/platform/systrap"
	"gvisor.dev/gvisor/pkg/sentry/seccheck"
	_ "gvisor.dev/gvisor/pkg/sentry/syscalls/linux"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sighandling"
)

var platformOnce sync.Once
var sharedPlatform platform.Platform
var platformErr error

// Systrap owns process-lifetime memory and stub pools. Each Sandbox owns a
// Kernel; its resident guest processes share that Kernel and platform.
func newKernel(p *observedPlatform) (*kernel.Kernel, error) {
	cpuid.Initialize()
	seccheck.Initialize()
	if err := rand.Init(); err != nil {
		return nil, err
	}
	if err := usage.Init(); err != nil {
		return nil, err
	}
	if err := sighandling.IgnoreChildStop(); err != nil {
		return nil, err
	}
	platformOnce.Do(func() {
		constructor, err := platform.Lookup("systrap")
		if err != nil {
			platformErr = err
			return
		}
		sharedPlatform, platformErr = constructor.New(platform.Options{})
	})
	if platformErr != nil {
		return nil, platformErr
	}
	p.Platform = sharedPlatform
	k := &kernel.Kernel{Platform: p}

	// A disk file lets idle processes release resident pages without losing
	// their virtual addresses. Unlink it immediately; Kernel owns its lifetime.
	memoryFile, err := os.CreateTemp("", "llar-runtime-memory-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(memoryFile.Name()); err != nil {
		memoryFile.Close()
		return nil, err
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(memoryFile.Fd()), &stat); err != nil {
		memoryFile.Close()
		return nil, err
	}
	mf, err := pgalloc.NewMemoryFile(memoryFile, pgalloc.MemoryFileOpts{DiskBackedFile: stat.Type != unix.TMPFS_MAGIC && stat.Type != unix.RAMFS_MAGIC, DecommitOnDestroy: true})
	if err != nil {
		memoryFile.Close()
		return nil, err
	}
	k.SetMemoryFile(mf)
	vdso, err := loader.PrepareVDSO(mf)
	if err != nil {
		return nil, fmt.Errorf("preparing VDSO: %w", err)
	}
	tk := kernel.NewTimekeeper()
	params := kernel.NewVDSOParamPage(mf, vdso.ParamPage.FileRange())
	tk.SetClocks(sentrytime.NewCalibratedClocks(false), params)
	userns := auth.NewRootUserNamespace()
	if err := k.Init(kernel.InitKernelArgs{
		FeatureSet: cpuid.HostFeatureSet().Fixed(),
		Timekeeper: tk, Vdso: vdso, VdsoParams: params,
		RootUserNamespace: userns,
		RootUTSNamespace:  kernel.NewUTSNamespace("", "", userns),
		RootIPCNamespace:  kernel.NewIPCNamespace(userns),
		RootPIDNamespace:  kernel.NewRootPIDNamespace(userns),
		ApplicationCores:  uint(runtime.GOMAXPROCS(0)),
		// Kernel.Init requires this even without container cgroup management.
		Cgroup2FSInit: cgroup2fs.NewFilesystem,
	}); err != nil {
		return nil, fmt.Errorf("initializing kernel: %w", err)
	}

	ctx := k.SupervisorContext()
	hostFS, err := host.NewFilesystem(k.VFS())
	if err != nil {
		return nil, err
	}
	defer hostFS.DecRef(ctx)
	k.SetHostMount(k.VFS().NewDisconnectedMount(hostFS, nil, &vfs.MountOptions{}))
	if err := registerFilesystems(k); err != nil {
		k.Release()
		return nil, err
	}
	return k, nil
}
