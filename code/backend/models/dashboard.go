package models

type DashboardStats struct {
	RepositoryCount int    `json:"repositoryCount"`
	JobCount        int    `json:"jobCount"`
	EnabledJobs     int    `json:"enabledJobs"`
	SuccessfulRuns  int    `json:"successfulRuns"`
	WarningRuns     int    `json:"warningRuns"`
	FailedRuns      int    `json:"failedRuns"`
	LastBackup      string `json:"lastBackup"`
	NextBackup      string `json:"nextBackup"`
}

type DashboardIssue struct {
	ID              string `json:"id"`
	Timestamp       string `json:"timestamp"`
	Kind            string `json:"kind"`
	IsNew           bool   `json:"isNew"`
	OperationKind   string `json:"operationKind,omitempty"`
	OperationID     string `json:"operationId,omitempty"`
	Severity        string `json:"severity"`
	Status          string `json:"status,omitempty"`
	Level           string `json:"level,omitempty"`
	StartedAt       string `json:"startedAt,omitempty"`
	FinishedAt      string `json:"finishedAt,omitempty"`
	Title           string `json:"title"`
	OutputAvailable bool   `json:"outputAvailable"`
}

type DashboardIssues struct {
	Items      []DashboardIssue `json:"items"`
	HasMore    bool             `json:"hasMore"`
	NextCursor string           `json:"nextCursor,omitempty"`
}
