//go:build linux

package engines

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

var linuxRcloneAuthorizationBrowserEnvironment = []string{
	"DISPLAY",
	"WAYLAND_DISPLAY",
	"XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS",
	"XAUTHORITY",
	"XDG_CURRENT_DESKTOP",
	"DESKTOP_SESSION",
	"WSL_INTEROP",
}

func rcloneAuthorizationBrowserEnvironment() ([]string, error) {
	environment := make([]string, 0, len(linuxRcloneAuthorizationBrowserEnvironment))
	present := make(map[string]bool, len(linuxRcloneAuthorizationBrowserEnvironment))
	for _, name := range linuxRcloneAuthorizationBrowserEnvironment {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			continue
		}
		if err := validateLinuxRcloneAuthorizationBrowserEnvironment(name, value); err != nil {
			return nil, err
		}
		environment = append(environment, name+"="+value)
		present[name] = true
	}
	if !present["DISPLAY"] && !present["WAYLAND_DISPLAY"] && !present["WSL_INTEROP"] {
		return nil, fmt.Errorf("native rclone browser authorization requires an active Linux graphical or WSL browser session")
	}
	if present["WAYLAND_DISPLAY"] &&
		!filepath.IsAbs(os.Getenv("WAYLAND_DISPLAY")) &&
		!present["XDG_RUNTIME_DIR"] {
		return nil, fmt.Errorf("native rclone browser authorization requires XDG_RUNTIME_DIR for the Wayland session")
	}
	return environment, nil
}

func validateLinuxRcloneAuthorizationBrowserEnvironment(name, value string) error {
	maximum := 4096
	switch name {
	case "DISPLAY", "WAYLAND_DISPLAY", "XDG_CURRENT_DESKTOP", "DESKTOP_SESSION":
		maximum = 512
	}
	if len(value) > maximum || value != strings.TrimSpace(value) ||
		strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("native rclone browser authorization found an invalid Linux session environment")
	}
	switch name {
	case "XDG_RUNTIME_DIR", "XAUTHORITY":
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("native rclone browser authorization found an invalid Linux session path")
		}
	case "WSL_INTEROP":
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("native rclone browser authorization found an invalid Linux session path")
		}
		info, err := os.Lstat(value)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("native rclone browser authorization requires an active WSL interop socket")
		}
	case "WAYLAND_DISPLAY":
		if filepath.IsAbs(value) {
			if filepath.Clean(value) != value {
				return fmt.Errorf("native rclone browser authorization found an invalid Wayland display")
			}
		} else if filepath.Base(value) != value || value == "." || value == ".." {
			return fmt.Errorf("native rclone browser authorization found an invalid Wayland display")
		}
	}
	return nil
}
