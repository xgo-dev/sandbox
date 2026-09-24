#ifndef LLAR_SANDBOX_BRIDGE_H
#define LLAR_SANDBOX_BRIDGE_H
// C ABI mirrored from sentry/sandbox.h. The library is loaded at runtime.
#include <stdint.h>
#include <stddef.h>

struct syscall_memory {
    void *data;
    uint64_t address;
    size_t length;
};
typedef char *(*mmap_memory_fn)(uintptr_t, uint64_t, size_t, struct syscall_memory *);
struct syscall_event {
    uint64_t number;
    uint64_t args[6];
    const char *name;
    uintptr_t context;
    mmap_memory_fn mmap;
    char *failure;
};
typedef void (*inspect_fn)(uintptr_t, struct syscall_event *);
typedef int (*create_sentry_fn)(uintptr_t *, char *, size_t);
typedef int (*close_sentry_fn)(uintptr_t, char *, size_t);
// Dispatches create/run/close for a process ID within a Kernel.
// Creation supplies guest/mount/env configuration; each run supplies a fresh image fd.
typedef int (*run_sentry_fn)(uintptr_t, char *, int, uintptr_t, uintptr_t, uintptr_t,
                             inspect_fn, char *, size_t);

static inline char *inspect_mmap(struct syscall_event *event, uint64_t address,
                                size_t size, struct syscall_memory *memory) {
    return event->mmap(event->context, address, size, memory);
}
int sandbox_create(char *, uintptr_t *, char *, size_t);
int sandbox_run(uintptr_t, char *, int, uintptr_t, uintptr_t, uintptr_t, char *, size_t);
int sandbox_close(uintptr_t, char *, size_t);
#endif
