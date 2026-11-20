package database

import "sync/atomic"

// dashboardChangeNotice holds the function set by NotifyDashboardChanges.
var dashboardChangeNotice atomic.Pointer[func()]

// NotifyDashboardChanges sets the function called after every database write
// that can change what the dashboard's "Active now" list (ListActiveOperations)
// or its Issues count (GetDashboardStats) shows: an operation queued, started,
// finished, interrupted at startup or pruned, a warning or error written to
// the activity log, the activity log cleared or pruned, and "Mark all as
// reviewed". The desktop tray uses it to update its icon without polling.
//
// notice is called on the writer's goroutine, after its write is committed,
// so it must return at once and must not touch the database itself. nil turns
// the notice off.
//
// A new write that changes either of those two queries has to call
// dashboardChanged too; otherwise the tray keeps showing the old state until
// the next write that does.
//
// One exception is deliberate: a Kopia policy process left stuck becomes an
// issue in GetDashboardStats from its saved state and the clock, not from a
// write, so nothing here announces it, and the writes of Kopia policy state
// are not hooked either. It is rare, and the tray catches up at the next
// notice of any kind or at once from a dashboard answer. Do not hook those
// writes or add a timer for it without a product decision.
func NotifyDashboardChanges(notice func()) {
	if notice == nil {
		dashboardChangeNotice.Store(nil)
		return
	}
	dashboardChangeNotice.Store(&notice)
}

// dashboardChanged is the one place the writers call after such a write.
func dashboardChanged() {
	if notice := dashboardChangeNotice.Load(); notice != nil {
		(*notice)()
	}
}
