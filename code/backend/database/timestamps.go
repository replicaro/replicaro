package database

import "time"

const sortableTimestampLayout = "2006-01-02T15:04:05.000000000Z"

func formatSortableTimestamp(value time.Time) string {
	return value.UTC().Format(sortableTimestampLayout)
}
