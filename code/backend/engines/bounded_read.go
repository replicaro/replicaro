package engines

import "context"

// ReadWithin runs read, a plain filesystem read of a vault path, and returns
// its result or ctx's error, whichever comes first. It exists because a stat,
// open, or directory listing on a hard-mounted NFS or dead SMB share can block
// in the kernel indefinitely, and no context can interrupt that call. Callers
// run these reads while holding a vault lock or the Kopia initialization lock
// (or while a user waits on a connection), so an unbounded read would keep
// the vault busy instead of letting the caller report a timeout and release
// the lock.
//
// When ctx ends first the read keeps running in its goroutine until the OS
// call returns on its own, and its result is dropped. The leftover goroutine
// keeps one OS thread busy until that call returns. It is tolerated because it
// holds no lock, and the storage probe that runs before these reads usually
// fails fast on a share that is still hung, so they rarely pile up.
//
// Only wrap reads. A write, rename, or removal that completes after the caller
// has returned and released its lock would change the vault behind the next
// lock holder's back.
//
// Every call starts its own read; results are deliberately never shared with
// or joined to a read started by another caller. Admission has to observe the
// vault after taking the lock, and an earlier read could predate a folder that
// has since been swapped or remounted. Do not "optimize" this into a shared
// in-flight read without keeping that revalidation.
//
// If ctx has already ended, no read is started.
func ReadWithin[T any](ctx context.Context, read func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	type result struct {
		value T
		err   error
	}
	// Buffered so a read that finishes after ctx has ended can still deliver
	// its result and exit instead of blocking forever.
	done := make(chan result, 1)
	go func() {
		value, err := read()
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		// A read that finished at the same moment still wins, so an expiring
		// deadline never hides evidence the read had already produced.
		select {
		case r := <-done:
			return r.value, r.err
		default:
			return zero, ctx.Err()
		}
	}
}
