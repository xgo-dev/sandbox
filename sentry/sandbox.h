#ifndef XGO_SANDBOX_H
#define XGO_SANDBOX_H
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

int CreateSandbox(uintptr_t *kernel, char *message, size_t capacity);
int CloseSandbox(uintptr_t kernel, char *message, size_t capacity);
int RunSandbox(uintptr_t kernel, char *config, int image_fd, uintptr_t main_pc, uintptr_t entry_pc,
                 uintptr_t owner, inspect_fn inspect, char *message, size_t capacity);

char *MMapSyscallMemory(uintptr_t, uint64_t, size_t, struct syscall_memory *);

static inline void prepare_inspection(struct syscall_event *event, uintptr_t context) {
    event->context = context;
    event->mmap = MMapSyscallMemory;
}

static inline void invoke_inspector(inspect_fn fn, uintptr_t owner,
                                    struct syscall_event *event) {
    fn(owner, event);
}
#endif
