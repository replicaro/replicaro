package schedulevalue

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const MaxCustomMinutes int64 = math.MaxInt64 / int64(time.Minute)

// Bound calendar intervals to the existing custom-duration horizon, using
// the longest month so even the maximum interval fits that horizon.
const MaxCustomMonths = MaxCustomMinutes / (31 * 24 * 60)
const MaxCronExpressionLength = 256

const cronPrefix = "cron:"

var fiveFieldCronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

func CustomMinutes(value string) (int64, bool) {
	const prefix = "every:"
	if !strings.HasPrefix(value, prefix) {
		return 0, false
	}
	digits := strings.TrimPrefix(value, prefix)
	if digits == "" {
		return 0, false
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	minutes, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || minutes <= 0 || minutes > MaxCustomMinutes {
		return 0, false
	}
	return minutes, true
}

// CustomMonths is a job-only calendar interval in the existing schedule field.
func CustomMonths(value string) (int, bool) {
	const prefix = "every-months:"
	if !strings.HasPrefix(value, prefix) {
		return 0, false
	}
	digits := strings.TrimPrefix(value, prefix)
	if digits == "" {
		return 0, false
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	months, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || months <= 0 || months > MaxCustomMonths {
		return 0, false
	}
	return int(months), true
}

func Valid(value string) bool {
	switch value {
	case "", "manual", "hourly", "daily", "weekly", "monthly":
		return true
	default:
		_, valid := CustomMinutes(value)
		return valid
	}
}

// NormalizeJob accepts the ordinary schedule vocabulary, job-only calendar
// intervals, and standard five-field cron expressions. Cron is never
// installed into the host's system cron service.
func NormalizeJob(value string) (string, bool) {
	if Valid(value) {
		return value, true
	}
	if _, valid := CustomMonths(value); valid {
		return value, true
	}
	if !strings.HasPrefix(value, cronPrefix) {
		return "", false
	}
	rawExpression := strings.TrimPrefix(value, cronPrefix)
	if len(rawExpression) > MaxCronExpressionLength {
		return "", false
	}
	expression := strings.TrimSpace(rawExpression)
	if expression == "" {
		return "", false
	}
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return "", false
	}
	expression = strings.Join(fields, " ")
	schedule, err := fiveFieldCronParser.Parse(expression)
	if err != nil || schedule.Next(time.Now()).IsZero() {
		return "", false
	}
	return cronPrefix + expression, true
}

func ValidJob(value string) bool {
	_, valid := NormalizeJob(value)
	return valid
}

func CronExpression(value string) (string, bool) {
	normalized, valid := NormalizeJob(value)
	if !valid || !strings.HasPrefix(normalized, cronPrefix) {
		return "", false
	}
	return strings.TrimPrefix(normalized, cronPrefix), true
}

func NextCron(value string, from time.Time) (time.Time, bool) {
	expression, valid := CronExpression(value)
	if !valid {
		return time.Time{}, false
	}
	schedule, err := fiveFieldCronParser.Parse(expression)
	if err != nil {
		return time.Time{}, false
	}
	next := schedule.Next(from.In(time.Local))
	return next, !next.IsZero()
}
