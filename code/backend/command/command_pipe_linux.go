//go:build linux

package command

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type commandOutputReader struct {
	interruptMu sync.Mutex
	pipe        *os.File
	failed      atomic.Bool
	remaining   int
	snapshotted bool
}

func newCommandOutputReader(pipe *os.File) *commandOutputReader {
	return &commandOutputReader{pipe: pipe}
}

func (reader *commandOutputReader) interrupt() {
	reader.interruptMu.Lock()
	defer reader.interruptMu.Unlock()
	if reader.failed.Load() {
		return
	}
	reader.failed.Store(true)
	// Wake a currently blocked Read. The reader then drains the bytes already
	// in the pipe, even if a slow capture/observer delayed its next Read.
	_ = reader.pipe.SetReadDeadline(time.Now())
}

func (reader *commandOutputReader) Read(buffer []byte) (int, error) {
	if !reader.failed.Load() {
		n, err := reader.pipe.Read(buffer)
		if n > 0 || !reader.failed.Load() || !errors.Is(err, os.ErrDeadlineExceeded) {
			return n, err
		}
	}
	// Wait for the interrupting caller to finish installing its wakeup
	// deadline before clearing it for buffered drainage.
	reader.interruptMu.Lock()
	reader.interruptMu.Unlock()
	connection, err := reader.pipe.SyscallConn()
	if err != nil {
		return 0, err
	}
	if !reader.snapshotted {
		var inspectErr error
		err = connection.Control(func(fd uintptr) { reader.remaining, inspectErr = unix.IoctlGetInt(int(fd), unix.TIOCINQ) })
		if err != nil {
			return 0, err
		}
		if inspectErr != nil {
			return 0, inspectErr
		}
		reader.snapshotted = true
		if err := reader.pipe.SetReadDeadline(time.Time{}); err != nil {
			return 0, err
		}
	}
	if reader.remaining == 0 {
		var events int16
		var pollErr error
		err = connection.Control(func(fd uintptr) {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			_, pollErr = unix.Poll(poll, 0)
			events = poll[0].Revents
		})
		if err != nil {
			return 0, err
		}
		if pollErr != nil {
			return 0, pollErr
		}
		if events&unix.POLLHUP != 0 && events&unix.POLLIN == 0 {
			return 0, io.EOF
		}
		return 0, errors.New("native output drainage incomplete after process cleanup failure")
	}
	if len(buffer) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	n, err := reader.pipe.Read(buffer)
	reader.remaining -= n
	return n, err
}
