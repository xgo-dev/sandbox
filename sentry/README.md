# Sentry Shared Library

This module builds `sentrylib.so`, the gVisor Sentry backend for the `github.com/xgo-dev/sandbox` library. It owns Sentry startup, the Systrap platform, configured filesystems and the context-switch inspection hook. The caller owns closure/value transfer and the guest entry.

The modules communicate through the C ABI in [sandbox.h](sandbox.h). There is no Go dependency in either direction. The host keeps its own copy of the ABI declarations in `bridge.h`; downloading the host Go module does not require these Sentry sources.

## Build

Build on Linux with Go 1.26.6, cgo and a C compiler for the target architecture:

```sh
bash build-linux.sh /tmp/llar-sandbox
```

The output contains `sentrylib.so`, the generated `sentrylib.h`, and `sandbox.h`. The Go host loads only the `.so` at runtime. Its `Sandbox.Library` field selects a path; the default is `sentrylib.so` beside the host executable.

## Releases

Pushing a tag matching `sentry/v*`, such as `sentry/v0.1.0`, runs GoReleaser to build both Linux architectures and upload these shared libraries directly to the matching GitHub Release after both builds succeed:

- `sentrylib-linux-amd64.so`
- `sentrylib-linux-arm64.so`

Builds use Go 1.26.6 in a Debian Bookworm container, with an ARM64 cross compiler. GoReleaser also uploads `checksums.txt`; C headers are available from a local build. Set `Sandbox.Library` to the downloaded file's path, or rename it to `sentrylib.so` beside the host executable. The runtime conditions below still apply.

The workflow can also be dispatched with an existing Sentry tag to retry a failed release. It takes the release configuration from the workflow commit and builds the source at the requested tag. The tag prefix is checked and HEAD must match that tag with a clean checkout; GoReleaser's own validation is skipped because its open-source edition does not parse `sentry/v*` as a semantic version.

## C Entry

```c
int CreateSandbox(uintptr_t *kernel, char *message, size_t capacity);
int RunSandbox(uintptr_t kernel, char *config, int image_fd, uintptr_t main_pc, uintptr_t entry_pc,
                 uintptr_t owner, inspect_fn inspect, char *message, size_t capacity);
int CloseSandbox(uintptr_t kernel, char *message, size_t capacity);
```

- `CreateSandbox` starts one Kernel and returns its opaque handle. `RunSandbox` creates a fresh process under that Kernel, with independent PID and mount namespaces, FD table and inspection state. Calls may overlap. The internal task identity is inherited by guest children so inspection and cleanup remain scoped to the originating Run.
- `CloseSandbox` rejects further calls, terminates tasks, waits for active runs and releases the Kernel. The caller must wait for its inspector callbacks to return; closing a Kernel synchronously from one of its own callbacks would deadlock.
- `config` is a NUL-terminated JSON object containing `guest` (the executable's absolute guest path), `mounts` and `env` (an array of `KEY=value` strings). Sentry starts the executable with that path as `argv[0]` and no extra arguments. Mount entries contain `type`, optional `source`, `target` and optional `options` (an array of strings). Environment entries are passed directly to the new process before runtime initialization; NUL is rejected. Missing, null or empty `env` gives an empty environment at this C entry. The Go host implements `Sandbox.Env == nil` inheritance by explicitly sending its own `os.Environ()`.
- `image_fd` is a caller-owned descriptor imported as guest fd 3. The library does not interpret its contents. Guest fd 0, 1 and 2 are imported from host stdin, stdout and stderr. Creation also installs an internal control socket at fd 4. The guest reads a byte with value 1 before loading fd 3 and sends the same byte after publishing its result. Fds 3 and 4 are close-on-exec.
- `main_pc` is the guest virtual address to redirect; the caller supplies its `main.main` address. The caller must verify that the symbol contains at least 5 bytes on AMD64 or 4 bytes on ARM64. Before starting guest tasks, the library writes a relative branch into this private executable mapping using Sentry's existing memory manager.
- `entry_pc` is the guest virtual address of the caller's private, non-capturing Go `func()` startup entry. It runs after Go package initialization and waits for successive task commands until the process closes. The caller owns ELF symbol resolution, guest code and value reconstruction. The library checks branch range and alignment; it does not interpret the closure image. Unsupported branch layouts fail before creating the guest.
- `owner` is an opaque integer passed unchanged to `inspect`. A Go caller can use a `cgo.Handle` owned by its own runtime.
- `inspect` is an optional synchronous callback. It receives a borrowed `syscall_event` containing the syscall number, name, six arguments and a memory mapping callback. The caller owns argument parsing and may change the registers directly. A null callback skips inspection setup. Guest pointer arguments must not be dereferenced in the host.
- `message` is a caller-owned writable error buffer of `capacity` bytes. For nonzero capacity, errors are truncated to at most `capacity - 1` bytes and NUL-terminated. No bytes are written at zero capacity. A nonzero result indicates an error; zero means that the process operation completed successfully.

Configuration strings and error buffers must remain valid until the operation returns. The inspector callback and its owner state must remain valid until the created process is closed, including between Run calls. Each event, its name and its context handle are borrowed only for the duration of `inspect`. Load one library per host process and keep it loaded: its Go runtime and Systrap workers retain executable code for the process lifetime, even after all Kernels are closed.

The lifecycle entry points and leading Kernel handle change the C ABI. Backends through `sentry/v0.5.0` are incompatible and lack `CreateSandbox`/`CloseSandbox`. Build both modules from the same source revision. Go module version selection does not validate a library loaded with `dlopen`.

Example process creation configuration:

```json
{
  "operation": "create",
  "process": 1,
  "guest": "/opt/llar/llar",
  "env": ["PATH=/usr/bin:/bin", "LANG=C"],
  "mounts": [
    {"type": "bind", "source": "/", "target": "/", "options": ["ro"]},
    {"type": "bind", "source": "/var/tmp/build", "target": "/work", "options": ["rw"]},
    {"type": "tmpfs", "target": "/tmp", "options": ["mode=1777", "size=256m"]},
    {"type": "proc", "target": "/proc"}
  ]
}
```

The backend requires an explicit list beginning with a `bind` or `tmpfs` root at `/`. The Go host supplies the default read-only `/` and guest `/proc` when its `Mounts` is empty. Bind sources must be absolute host directory paths. Targets are absolute guest paths; mounts are applied in order, and duplicate targets are rejected. Parent mounts and overlay layers must precede their users. Missing directory mountpoints use Sentry's synthetic-mountpoint support and require a writable parent mount; targets under a read-only parent must already exist.

Registered types are `bind` (translated to gofer with DirectFS), `tmpfs`, `proc` and `overlay`. Common options are `ro`/`rw`, `noexec`/`exec`, `nosuid`/`suid` and `noatime`/`atime`, with the last option in a pair taking precedence. Tmpfs and overlay filesystem options are passed to Sentry as mount data; unsupported options fail. For overlay, `lowerdir` and `upperdir` refer to paths in the guest namespace, not host paths. An upper tmpfs makes changes temporary; use writable binds for persistent outputs. Mount namespaces, tmpfs contents and bind connections live until the process closes; partial setup failures also release them.

The event's `mmap(context, address, size, memory)` callback allocates anonymous temporary guest pages using `MMap` and pins them. Address `0` requests `size` zeroed bytes without a source; a nonzero address initializes the pages using `CopyIn`. Sentry chooses a vacant guest address. The returned `syscall_memory` contains a host `data` pointer directly mapping those pages, their guest `address`, and the initialized `length`. The host and guest addresses need not match. Editing `data` immediately changes the temporary pages without changing the original guest bytes. There is no caller-owned staging buffer or write/commit callback; initialization from a source copies bytes and is not COW. Address-zero allocation requires `sentry/v0.3.0` or later; it is not supported by `sentry/v0.2.0`.

**Pointer safety:** New syscall buffers must be allocated through the memory callback, and injected pointer arguments and nested pointers must use the returned guest `address`. Never inject host Go/C addresses, including a `malloc`/`CString` pointer or the returned host `data` pointer. Copy intended payload bytes into `data`, but encode embedded pointers as guest addresses. Sentry resolves syscall pointers in the guest address space: a host pointer may cause `EFAULT`, access unrelated guest memory, or leak a host address. This is a security requirement for the interceptor; Sentry does not identify host-pointer provenance or automatically relocate values.

Memory access expires when `inspect` returns; the data pointer must not escape that callback, and the mapped contents must not contain host Go or C pointers. A partial read returns the initialized prefix and an optional C-allocated error string, which the caller frees with `free`. An inspection `failure` is a C-allocated error string consumed and freed by Sentry; it aborts execution and releases temporary mappings.

[inspect_linux.go](inspect_linux.go) implements the memory callback. [memory_linux.go](memory_linux.go) uses `MapInternal` to expose pinned pages; when those pages are fragmented, it maps their backing file ranges into a contiguous host reservation. It wraps registered syscall functions once to release temporary mappings after execution. Restarted calls and calls skipped before dispatch are retained until kernel teardown. The wrapper does not decode arguments or copy outputs to original guest buffers. Temporary addresses must not be retained by the syscall or unmapped/remapped by guest threads; these operations are not currently prevented.

```text
guest syscall
    -> seccomp / SIGSYS / sysmsg
    -> underlying Context.Switch returns
    -> host inspection callback
       -> MMap: MMap + Pin; CopyIn only for nonzero source addresses
       <- host alias + guest address
       -> edit temporary pages and explicitly replace arguments/pointers
    -> updated registers
    -> Sentry syscall dispatch
    -> release temporary mappings
    -> next Context.Switch resumes the guest
```

The hook is implemented in [platform_linux.go](platform_linux.go). It does not modify gVisor's sysmsg queues, futex handoff or kernel syscall implementations. [run_linux.go](run_linux.go) owns persistent guest processes within each Kernel. A completed run stops only its process; other guests remain runnable. The Systrap platform is retained between Kernels.

## Process operations and idle memory

`RunSandbox` dispatches `create`, `run` and `close` operations from the configuration JSON, keyed by `process` within its Kernel. Creation installs the guest entry and inspector. Run replaces fd 3 while the guest is paused, wakes it, exchanges a command/completion byte on fd 4 and waits for its tasks to stop again. Close interrupts active control I/O, terminates the process and waits for its threads, mappings and filesystem services to release. CloseSandbox closes all of its processes before releasing the Kernel. This operation protocol requires host and backend from the same source revision, even when an older backend exports the same C symbol names.

Each process schedules idle eviction ten seconds after completing a run. A new run cancels the previous idle timer; timer callbacks verify their identity under the process lock so an old timer cannot evict a later call. The Kernel's application MemoryFile uses an unlinked temporary backing file. Eviction holds the selected memory manager's mapping lock, flushes existing private non-COW ranges, removes platform mappings and requests file-cache reclamation. It retains VMA/PMA and thread state, and never calls Decommit on live data. The ordinary fault path reloads pages after resume. The pinned gVisor dependency remains unchanged; `process_memory_linux.go` adapts its private mapping metadata using reflection and its existing methods. Host temporary storage must be disk-backed for this reclamation to work. This mechanism retains kernel/FD/VFS state and is not a standalone checkpoint.

## Runtime Conditions

Start the host with `GLIBC_TUNABLES=glibc.pthread.rseq=0`. Systrap's ptrace/seccomp initialization must be permitted by the surrounding environment. The guest uses UID/GID 1000 and working directory `/`; its environment comes from the startup `env` array. Bind mounts use in-process LISAFS services for DirectFS. The executable, loader and shared libraries must be visible in the configured namespace. Filesystem permissions still apply in addition to mount flags.

Native execution has been verified on Linux ARM64 with 4 KiB pages, including LLAR formula execution and syscall rewriting through the host callback. AMD64 builds are verified; native AMD64 Sentry execution and other page sizes still need validation. Both Go runtimes share the host OS address space and signal dispositions. Dependency separation through c-shared is not itself a memory protection boundary inside the host.

The startup code in `boot_linux.go` and `fs_linux.go` is adapted from gVisor Go-export commit `d1e35511e5a41ee5c2afc522d2f9b0c27cf8d382`; original notices and the Apache 2.0 license are retained. The gVisor dependency is pinned in `go.mod`, and its sources are unmodified.
