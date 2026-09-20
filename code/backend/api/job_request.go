package api

import "github.com/local/replicaro/models"

type JobRequest struct {
	ID                      string                `json:"id"`
	Name                    string                `json:"name"`
	Source                  string                `json:"source"`
	RepositoryIDs           []string              `json:"repositoryIds"`
	Schedule                string                `json:"schedule"`
	Retention               int                   `json:"retention"`
	RetentionHourly         *int                  `json:"retentionHourly"`
	RetentionDaily          *int                  `json:"retentionDaily"`
	RetentionWeekly         *int                  `json:"retentionWeekly"`
	RetentionMonthly        *int                  `json:"retentionMonthly"`
	RetentionYearly         *int                  `json:"retentionYearly"`
	Excludes                string                `json:"excludes"`
	Tag                     string                `json:"tag"`
	BeforeScriptPath        string                `json:"beforeScriptPath"`
	BeforeScriptMustSucceed bool                  `json:"beforeScriptMustSucceed"`
	AfterScriptPath         string                `json:"afterScriptPath"`
	AfterScriptMustSucceed  bool                  `json:"afterScriptMustSucceed"`
	EngineSettings          models.EngineSettings `json:"engineSettings"`
	Enabled                 *bool                 `json:"enabled"`
}
