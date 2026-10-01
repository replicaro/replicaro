package api

// Vault creation, connection, credential rotation and cloud sign-in apply run
// inside their HTTP request and have no operation record. When their main
// change has committed (the vault is attached, or the new credentials are
// saved) but a cleanup step after that did not finish, the request still
// succeeds, and this marks its response as completed with issues. The status
// value is the same one tracked operations use, so the UI reads one
// vocabulary.
//
// Do not turn these cases back into errors (500) or 202s: the change is live
// and retrying the request would redo committed work. And do not report them
// as plain success: the cleanup left behind (a local creation fence, a
// temporary sign-in session, old Kopia config, quarantined engine artifacts)
// still needs attention, and the issue text says what.
//
// A failure before the commit point is still a failure, never this.
const foregroundCompletedWithIssues = "completed_with_issues"

type foregroundOutcome struct {
	Status string   `json:"status,omitempty"`
	Issues []string `json:"issues,omitempty"`
}

func (o *foregroundOutcome) addIssue(message string) {
	o.Status = foregroundCompletedWithIssues
	o.Issues = append(o.Issues, message)
}

// addTo copies the outcome into a map-shaped success response. A clean
// success adds nothing, so those responses stay exactly as they were.
func (o foregroundOutcome) addTo(response map[string]any) {
	if o.Status == "" {
		return
	}
	response["status"] = o.Status
	response["issues"] = o.Issues
}
