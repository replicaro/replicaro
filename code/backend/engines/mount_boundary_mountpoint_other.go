//go:build !windows && !linux

package engines

func destinationMountSpecific(string) error { return nil }
