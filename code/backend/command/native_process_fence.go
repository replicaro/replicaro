package command

import (
	"context"
	"fmt"
	"os"
	"time"
)

type nativeProcessFenceKey struct{}

// NativeProcessFence is a cross-process proof token. On supported platforms the
// native command and every descendant inherit its locked file descriptor.
type NativeProcessFence struct {
	file      *os.File
	supported bool
	proofKind string
	release   func() error
}

func AcquireNativeProcessFence(path string) (*NativeProcessFence, error) {
	if path == "" {
		return nil, fmt.Errorf("native operation fence path is required")
	}
	return acquireNativeProcessFence(path)
}

func (fence *NativeProcessFence) Supported() bool { return fence != nil && fence.supported }
func (fence *NativeProcessFence) ProofKind() string {
	if fence == nil || !fence.supported {
		return "unsupported"
	}
	return fence.proofKind
}

func (fence *NativeProcessFence) Close() error {
	if fence == nil {
		return nil
	}
	if fence.release != nil {
		return fence.release()
	}
	if fence.file == nil {
		return nil
	}
	return fence.file.Close()
}

func WithNativeProcessFence(ctx context.Context, fence *NativeProcessFence) context.Context {
	return context.WithValue(ctx, nativeProcessFenceKey{}, fence)
}

func nativeProcessFenceFromContext(ctx context.Context) *NativeProcessFence {
	fence, _ := ctx.Value(nativeProcessFenceKey{}).(*NativeProcessFence)
	return fence
}

// NativeProcessFenceInactive reports true only when no process still holds the
// operation's inherited lease. Unsupported platforms deliberately never can.
func NativeProcessFenceInactive(path string) (bool, error) {
	return nativeProcessFenceInactive(path)
}

// CleanupClosedNativeProcessFence removes the fence file only when no process
// still holds its lock. Windows uses process-global Job Objects and has no fence file.
func CleanupClosedNativeProcessFence(path string) error {
	if path == "" {
		return fmt.Errorf("native operation fence path is required")
	}
	return cleanupClosedNativeProcessFence(path)
}

// A check made right after this process closed its own fence handle can see
// the lock as still held even though the native command tree is gone. Any
// other process launch in Replicaro (a scheduled backup, a size refresh, a
// helper) clones the parent's whole descriptor table, and the fence
// descriptor stays in that child until exec closes it (close-on-exec). With
// several launches in flight, back-to-back vault creations hit a false "still
// active" about once in fifteen, and it always cleared within a few
// milliseconds. The callers that close and then check therefore look again
// for a short, bounded window before believing the fence is held. A native
// process that really keeps the fence holds it for far longer than this
// window and is still reported active. The window is not a wait for native
// work to finish, so do not stretch it into one.
var (
	closedFenceSettleWindow   = 1500 * time.Millisecond
	closedFenceSettleInterval = 20 * time.Millisecond
)

// awaitClosedNativeProcessFence repeats the inactivity check until it passes,
// fails with an error, or the settle window runs out. Errors are returned
// straight away; only a "held" answer is looked at again.
func awaitClosedNativeProcessFence(path string) (bool, error) {
	deadline := time.Now().Add(closedFenceSettleWindow)
	for {
		inactive, err := nativeProcessFenceInactive(path)
		if err != nil || inactive || !time.Now().Before(deadline) {
			return inactive, err
		}
		time.Sleep(closedFenceSettleInterval)
	}
}

// NativeProcessFenceInactiveAfterClose is NativeProcessFenceInactive for a
// check that follows closely on closing the fence (or on another request that
// may just have closed it). It tolerates the brief false "held" caused by
// unrelated process launches and otherwise gives the same answer.
func NativeProcessFenceInactiveAfterClose(path string) (bool, error) {
	return awaitClosedNativeProcessFence(path)
}

// CleanupNativeProcessFenceAfterClose is CleanupClosedNativeProcessFence for
// a fence this process has only just closed, with the same short tolerance.
func CleanupNativeProcessFenceAfterClose(path string) error {
	if path == "" {
		return fmt.Errorf("native operation fence path is required")
	}
	return cleanupNativeProcessFenceAfterClose(path)
}
