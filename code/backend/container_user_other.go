//go:build !linux

package main

import "fmt"

// The container package is Linux only, and requireContainerRuntime refuses
// other systems before this is reached.
func settleContainerUser(bool, *containerUser) error {
	return fmt.Errorf("--container-published-port requires a Linux container runtime")
}
