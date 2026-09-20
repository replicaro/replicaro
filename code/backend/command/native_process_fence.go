package command

import (
	"context"
	"fmt"
	"os"
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

// NativeProcessFenceInactive proves that no process retains the exact operation's
// inherited lease. Unsupported platforms deliberately cannot make that proof.
func NativeProcessFenceInactive(path string) (bool, error) {
	return nativeProcessFenceInactive(path)
}

// CleanupClosedNativeProcessFence removes only an inactive filesystem proof.
// Windows uses process-global Job Objects and has no fence-path artifact.
func CleanupClosedNativeProcessFence(path string) error {
	if path == "" {
		return fmt.Errorf("native operation fence path is required")
	}
	return cleanupClosedNativeProcessFence(path)
}
