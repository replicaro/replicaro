package models

type Operation struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"` // queued, running, success, completed_with_issues, reconnect_required, failed, partial, interrupted, skipped
	Title        string          `json:"title"`
	JobID        string          `json:"jobId"`
	RepositoryID string          `json:"repositoryId"`
	Engine       string          `json:"engine"`
	StartedAt    string          `json:"startedAt"`
	FinishedAt   string          `json:"finishedAt"`
	Steps        []OperationStep `json:"steps,omitempty"`
}

type OperationStep struct {
	ID          string `json:"id"`
	OperationID string `json:"operationId"`
	Domain      string `json:"domain"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	StartedAt   string `json:"startedAt"`
	FinishedAt  string `json:"finishedAt"`
}
