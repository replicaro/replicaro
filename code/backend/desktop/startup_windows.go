//go:build windows

package desktop

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const (
	startupKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	startupValue   = "Replicaro"
	legacyValue    = "Rewind Companion"
)

func RegisterStartup() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}

	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}

	key, _, err := registry.CreateKey(
		registry.CURRENT_USER,
		startupKeyPath,
		registry.SET_VALUE,
	)
	if err != nil {
		return err
	}
	defer key.Close()

	if err := key.SetStringValue(startupValue, `"`+executable+`"`); err != nil {
		return err
	}
	_ = key.DeleteValue(legacyValue)
	return nil
}

func UnregisterStartup() error {
	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		startupKeyPath,
		registry.SET_VALUE,
	)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()

	for _, value := range []string{startupValue, legacyValue} {
		if err := key.DeleteValue(value); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
	}
	return nil
}
