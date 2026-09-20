//go:build !linux

package engines

func rcloneAuthorizationBrowserEnvironment() ([]string, error) {
	return nil, nil
}
