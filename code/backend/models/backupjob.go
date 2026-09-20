package models

type BackupJob struct {
	ID                          string            `json:"id"`
	Name                        string            `json:"name"`
	Source                      string            `json:"source"`
	SourceStorageVersion        string            `json:"-"`
	SourceStorageKey            string            `json:"-"`
	SourceStorageDescriptorJSON string            `json:"-"`
	SourceBindingState          string            `json:"sourceBindingState,omitempty"`
	ResolvedSourcePath          string            `json:"resolvedSourcePath,omitempty"`
	ResolvedSourceObservedAt    string            `json:"resolvedSourceObservedAt,omitempty"`
	Targets                     []BackupJobTarget `json:"targets"`
	Schedule                    string            `json:"schedule"` // manual|hourly|daily|weekly|monthly|every:<minutes>|every-months:<N>|cron:<five fields>
	Enabled                     bool              `json:"enabled"`
	NextRun                     string            `json:"nextRun"`
	LastRun                     string            `json:"lastRun"`
	Retention                   int               `json:"retention"` // keep last N snapshots for this source; 0 = keep all
	RetentionHourly             *int              `json:"retentionHourly,omitempty"`
	RetentionDaily              *int              `json:"retentionDaily,omitempty"`
	RetentionWeekly             *int              `json:"retentionWeekly,omitempty"`
	RetentionMonthly            *int              `json:"retentionMonthly,omitempty"`
	RetentionYearly             *int              `json:"retentionYearly,omitempty"`
	Excludes                    string            `json:"excludes"` // newline-separated glob patterns
	Tag                         string            `json:"tag"`
	BeforeScriptPath            string            `json:"beforeScriptPath"`
	BeforeScriptMustSucceed     bool              `json:"beforeScriptMustSucceed"`
	AfterScriptPath             string            `json:"afterScriptPath"`
	AfterScriptMustSucceed      bool              `json:"afterScriptMustSucceed"`
	EngineSettings              EngineSettings    `json:"engineSettings"`
	PortableTargetIDs           []string          `json:"-"`
	SizeBytes                   *int64            `json:"sizeBytes"`
	SizeMeasuredAt              string            `json:"sizeMeasuredAt"`
}

type EngineSettings map[string]EngineJobSettings

type EngineJobSettings struct {
	AdditionalOptions []string `json:"additionalOptions,omitempty"`
}

type BackupJobTarget struct {
	RepositoryID             string `json:"repositoryId"`
	RepositoryName           string `json:"repositoryName"`
	Engine                   string `json:"engine"`
	LastRun                  string `json:"lastRun"`
	LastStatus               string `json:"lastStatus"`
	OperationID              string `json:"operationId,omitempty"`
	PendingCatchUp           bool   `json:"pendingCatchUp"`
	CoalescedMissedCount     int64  `json:"coalescedMissedCount"`
	FirstDeferredDueAt       string `json:"firstDeferredDueAt"`
	LastDueAt                string `json:"lastDueAt"`
	SourceAvailability       string `json:"sourceAvailability"`
	SourceAvailabilityReason string `json:"sourceAvailabilityReason"`
	SourceAvailabilityCheck  string `json:"sourceAvailabilityCheckedAt"`
	TargetAvailability       string `json:"targetAvailability"`
	TargetAvailabilityReason string `json:"targetAvailabilityReason"`
	TargetAvailabilityCheck  string `json:"targetAvailabilityCheckedAt"`
	PolicyStatus             string `json:"policyStatus,omitempty"`
	PolicyError              string `json:"policyError,omitempty"`
	LogicalSizeBytes         *int64 `json:"-"`
	LogicalSizeMeasuredAt    string `json:"-"`
}

type BackupTargetStatus struct {
	JobID          string `json:"jobId"`
	RepositoryID   string `json:"repositoryId"`
	RepositoryName string `json:"repositoryName"`
	OperationID    string `json:"operationId"`
	Status         string `json:"status"`
}
