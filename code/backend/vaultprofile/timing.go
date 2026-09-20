package vaultprofile

import (
	"context"
	"strings"
	"time"
)

// TimingReporter receives safe, non-secret timings for remote profile work.
// Labels contain only fixed operation names and profile generation names.
type TimingReporter func(label string, elapsed time.Duration)

type timingReporterContextKey struct{}

// WithTimingReporter enables diagnostics for profile operations performed with
// the returned context.
func WithTimingReporter(ctx context.Context, reporter TimingReporter) context.Context {
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, timingReporterContextKey{}, reporter)
}

func reportTiming(ctx context.Context, label string, elapsed time.Duration) {
	reporter, _ := ctx.Value(timingReporterContextKey{}).(TimingReporter)
	if reporter != nil {
		reporter(label, elapsed)
	}
}

func profileTimingLabel(args []string) string {
	if len(args) == 0 {
		return "profile remote command"
	}

	objectLabel := func(value string) string {
		name := strings.TrimPrefix(value, "crypt:")
		switch {
		case name == canonicalProfileObject:
			return "canonical"
		case name == previousProfileObject:
			return "previous"
		case strings.HasPrefix(name, pendingProfilePrefix):
			return "pending"
		default:
			return "object"
		}
	}

	switch args[0] {
	case "lsjson":
		if len(args) > 1 && args[1] == "crypt:" {
			return "profile rclone lsjson recovery directory"
		}
		return "profile rclone lsjson root"
	case "cat":
		if len(args) > 1 {
			return "profile rclone cat " + objectLabel(args[1])
		}
		return "profile rclone cat object"
	case "copyto":
		if len(args) > 2 {
			switch objectLabel(args[2]) {
			case "pending":
				return "profile publication copy pending"
			case "previous":
				return "profile publication preserve previous"
			case "canonical":
				return "profile publication copy canonical"
			}
		}
		return "profile rclone copy"
	case "deletefile":
		if len(args) > 1 {
			return "profile publication cleanup " + objectLabel(args[1])
		}
		return "profile publication cleanup object"
	default:
		return "profile rclone " + args[0]
	}
}
