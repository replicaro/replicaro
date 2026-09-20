//go:build windows

package engines

var destinationMountDetector = func(string) error { return nil }

func validateDestinationMounts(string) error { return nil }
