//go:build linux && (amd64 || arm64) && cgo

package main

import (
	"errors"
	"fmt"
	"reflect"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// evict is called with p.mu held after every task in this process has stopped.
// It retains VMAs, PMAs and thread state; the ordinary fault path reloads pages.
func (p *guestProcess) evict() error {
	ctx := p.kernel.SupervisorContext()
	managers := make(map[*mm.MemoryManager]struct{})
	for _, task := range p.kernel.RootPIDNamespace().Tasks() {
		if task.ContainerID() != p.id {
			continue
		}
		task.WithMuLocked(func(task *kernel.Task) {
			m := task.MemoryManager()
			if m == nil {
				return
			}
			if _, exists := managers[m]; exists {
				return
			}
			if m.IncUsers() {
				managers[m] = struct{}{}
			}
		})
	}
	defer func() {
		for m := range managers {
			m.DecUsers(ctx)
		}
	}()
	var err error
	for m := range managers {
		err = errors.Join(err, evictPrivateMemory(ctx, m, p.kernel.MemoryFile()))
	}
	return err
}

// gVisor d1e35511e5a4 keeps its resident mappings in MemoryManager.pmas.
// Pin would instantiate untouched reservations and break COW, so read the
// existing set through its ExportSlice method while holding its own activeMu.
// Reflection avoids duplicating the generated segment-tree layout. This adapter
// is local to Sentry; gVisor's source and public API remain unchanged.
func evictPrivateMemory(ctx context.Context, m *mm.MemoryManager, mf *pgalloc.MemoryFile) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("gVisor private mapping metadata: %v", v)
		}
	}()
	if !mf.IsDiskBacked() {
		return errors.New("guest memory backing is not a disk file")
	}
	value := reflect.ValueOf(m).Elem()
	lockField := value.FieldByName("activeMu")
	lock := reflect.NewAt(lockField.Type(), lockField.Addr().UnsafePointer()).Interface().(interface {
		Lock()
		Unlock()
	})
	// Keep kernel-side memory I/O and mapping invalidation out of this operation,
	// including asynchronous I/O that does not execute a guest task.
	lock.Lock()
	defer lock.Unlock()
	set := value.FieldByName("pmas")
	flat := reflect.NewAt(set.Type(), set.Addr().UnsafePointer()).MethodByName("ExportSlice").Call(nil)[0]
	var pins []mm.PinnedRange
	defer func() { mm.Unpin(pins) }()
	for i := 0; i < flat.Len(); i++ {
		segment := flat.Index(i)
		page := segment.FieldByName("Value")
		if !page.FieldByName("private").Bool() || page.FieldByName("needCOW").Bool() {
			continue
		}
		fileField := page.FieldByName("file")
		file := reflect.NewAt(fileField.Type(), fileField.Addr().UnsafePointer()).Elem().Interface().(memmap.File)
		if file != mf {
			return errors.New("private mapping has an unexpected backing file")
		}
		pin := mm.PinnedRange{Source: hostarch.AddrRange{Start: hostarch.Addr(segment.FieldByName("Start").Uint()), End: hostarch.Addr(segment.FieldByName("End").Uint())}, File: file, Offset: page.FieldByName("off").Uint()}
		file.IncRef(pin.FileRange(), pgalloc.MemoryCgroupIDFromContext(ctx))
		pins = append(pins, pin)
	}
	// Complete writeback before removing any mappings. Decommit is deliberately
	// not used: it punches holes and destroys the stored guest contents.
	for _, pin := range pins {
		blocks, e := mf.MapInternal(pin.FileRange(), hostarch.ReadWrite)
		if e != nil {
			return e
		}
		for !blocks.IsEmpty() {
			if e := unix.Msync(blocks.Head().ToSlice(), unix.MS_SYNC); e != nil {
				return e
			}
			blocks = blocks.Tail()
		}
	}
	for _, pin := range pins {
		m.AddressSpace().Unmap(pin.Source.Start, uint64(pin.Source.Length()))
		blocks, e := mf.MapInternal(pin.FileRange(), hostarch.ReadWrite)
		if e != nil {
			return e
		}
		for !blocks.IsEmpty() {
			if e := unix.Madvise(blocks.Head().ToSlice(), unix.MADV_DONTNEED); e != nil {
				return e
			}
			blocks = blocks.Tail()
		}
		fr := pin.FileRange()
		if e := unix.Fadvise(mf.FD(), int64(fr.Start), int64(fr.Length()), linux.POSIX_FADV_DONTNEED); e != nil {
			return e
		}
	}
	return nil
}
