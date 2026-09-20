package engines

// RcloneConfigDisposition records the run-local ownership result of one
// attempted native config activation. It is deliberately not persisted.
type RcloneConfigDisposition string

const (
	RcloneConfigRetained      RcloneConfigDisposition = "retained"
	RcloneConfigActivated     RcloneConfigDisposition = "activated"
	RcloneConfigIndeterminate RcloneConfigDisposition = "indeterminate"
)

// RcloneConfigActivation distinguishes native path activation from later
// database or product attachment. Only Retained remains retryable.
type RcloneConfigActivation struct {
	Path        string                  `json:"-"`
	Disposition RcloneConfigDisposition `json:"disposition"`
}

func (activation RcloneConfigActivation) ConsumesAuthorization() bool {
	return activation.Disposition == RcloneConfigActivated ||
		activation.Disposition == RcloneConfigIndeterminate
}

func retainedRcloneConfig(target string) RcloneConfigActivation {
	return RcloneConfigActivation{Path: target, Disposition: RcloneConfigRetained}
}
