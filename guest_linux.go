//go:build linux && (arm64 || amd64) && cgo

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/xgo-dev/sandbox/internal/state"
	"golang.org/x/sys/unix"
)

// guestEntry replaces main.main in the guest after Go package initialization.
// Its address is passed by Run, keeping this entry and its dependencies linked.
//
//go:noinline
func guestEntry() {
	if err := runGuest(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runGuest() (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("sandbox guest panicked: %v", value)
		}
	}()
	control := os.NewFile(4, "sandbox-control")
	defer control.Close()
	defer unix.Close(3)
	var graph state.State
	var fn func()
	var command [1]byte
	for {
		if _, err := io.ReadFull(control, command[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if command[0] != 1 {
			return errors.New("sandbox: invalid process command")
		}
		data, unmap, err := readStateImage(3, 0)
		if err != nil {
			return err
		}
		resultOffset := int64(len(data)) + 8
		_, loadErr := graph.Load(context.Background(), data, &fn)
		err = errors.Join(loadErr, unmap())
		if err != nil {
			return err
		}
		runtime.GC()
		fn()
		if _, err := writeStateImage(3, resultOffset, &graph, &fn); err != nil {
			return err
		}
		if _, err := control.Write(command[:]); err != nil {
			return err
		}
	}
}
