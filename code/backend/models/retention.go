package models

import "fmt"

const MaxRetentionCount = 2_147_483_647

func ValidateRetentionPolicy(job BackupJob) error {
	if job.Retention < 0 {
		return fmt.Errorf("retention cannot be negative")
	}
	for _, tier := range []struct {
		name  string
		value *int
	}{
		{"hourly", job.RetentionHourly},
		{"daily", job.RetentionDaily},
		{"weekly", job.RetentionWeekly},
		{"monthly", job.RetentionMonthly},
		{"yearly", job.RetentionYearly},
	} {
		name, value := tier.name, tier.value
		if value != nil && (*value < 1 || *value > MaxRetentionCount) {
			return fmt.Errorf("%s retention must be between 1 and %d when set", name, MaxRetentionCount)
		}
	}
	return nil
}

func RetentionPolicyEqual(left, right BackupJob) bool {
	return left.Retention == right.Retention &&
		optionalIntEqual(left.RetentionHourly, right.RetentionHourly) &&
		optionalIntEqual(left.RetentionDaily, right.RetentionDaily) &&
		optionalIntEqual(left.RetentionWeekly, right.RetentionWeekly) &&
		optionalIntEqual(left.RetentionMonthly, right.RetentionMonthly) &&
		optionalIntEqual(left.RetentionYearly, right.RetentionYearly)
}

func optionalIntEqual(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
