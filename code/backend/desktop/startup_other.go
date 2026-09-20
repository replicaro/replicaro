//go:build !windows

package desktop

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func RegisterStartup() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = startupExecutable(executable)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		if isMacOSAppTranslocatedPath(executable) {
			return errors.New("Replicaro is running from a macOS AppTranslocation location; move the app to a permanent location and launch it again before enabling start at login")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path := filepath.Join(home, "Library", "LaunchAgents", "io.replicaro.desktop.plist")
		content := managedLaunchAgentContent(executable, true)
		legacy := managedLaunchAgentContent(executable, false)
		return writeManagedLaunchAgentFile(path, []byte(content), []byte(legacy))
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	serviceDir := filepath.Join(config, "systemd", "user")
	if _, err := exec.LookPath("systemctl"); err == nil {
		path := filepath.Join(serviceDir, "replicaro.service")
		unitOwned := true
		if existing, readErr := os.ReadFile(path); readErr == nil && !strings.HasPrefix(string(existing), "# Managed by Replicaro\n") && string(existing) != managedSystemdUnit(executable, false) {
			unitOwned = false
		} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if !unitOwned {
			// Never enable, replace, or remove an unowned user unit.
			goto desktopFallback
		}
		if hasVendorSystemdUnit() {
			// Package-managed units are authoritative. Remove only an override
			// previously written by Replicaro, never a user's custom unit.
			if err := removeManagedSystemdOverride(path, executable); err != nil {
				return err
			}
		} else {
			content := managedSystemdUnit(executable, true)
			if err := writeManagedStartupFile(path, []byte(content), 0o600, managedSystemdUnit(executable, false)); err != nil {
				return err
			}
		}
		if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err == nil {
			if err := exec.Command("systemctl", "--user", "enable", "replicaro.service").Run(); err == nil {
				return nil
			}
		}
	}
desktopFallback:
	desktopPath := filepath.Join(config, "autostart", "replicaro.desktop")
	content := "# Managed by Replicaro\n[Desktop Entry]\nType=Application\nName=Replicaro\nExec=" + desktopEscape(executable) + "\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
	legacy := "[Desktop Entry]\nType=Application\nName=Replicaro\nExec=" + desktopEscape(executable) + "\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
	return writeManagedStartupFile(desktopPath, []byte(content), 0o600, legacy)
}

func isMacOSAppTranslocatedPath(path string) bool {
	if path == "" {
		return false
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == "AppTranslocation" {
			return true
		}
	}
	return false
}

func startupExecutable(runningExecutable string) (string, error) {
	executable, err := filepath.Abs(runningExecutable)
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "linux" {
		return executable, nil
	}
	appImage, appDir := os.Getenv("APPIMAGE"), os.Getenv("APPDIR")
	if appImage == "" || appDir == "" || !filepath.IsAbs(appImage) || !filepath.IsAbs(appDir) {
		return executable, nil
	}
	relative, err := filepath.Rel(filepath.Clean(appDir), executable)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return executable, nil
	}
	info, err := os.Stat(appImage)
	if err != nil || info.IsDir() {
		return "", errors.New("resolve original AppImage path for start-at-login")
	}
	return filepath.Clean(appImage), nil
}

func UnregisterStartup() error {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		executable, _ := os.Executable()
		executable, _ = startupExecutable(executable)
		return removeManagedLaunchAgentFile(filepath.Join(home, "Library", "LaunchAgents", "io.replicaro.desktop.plist"), executable)
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	executable, _ := os.Executable()
	if executable != "" {
		executable, _ = startupExecutable(executable)
	}
	unitPath := filepath.Join(config, "systemd", "user", "replicaro.service")
	unitData, unitErr := os.ReadFile(unitPath)
	unitManaged := unitErr == nil && (strings.HasPrefix(string(unitData), "# Managed by Replicaro\n") || (executable != "" && string(unitData) == managedSystemdUnit(executable, false)))
	if errors.Is(unitErr, os.ErrNotExist) && hasVendorSystemdUnit() {
		unitManaged = true
	}
	if unitManaged {
		_ = exec.Command("systemctl", "--user", "disable", "replicaro.service").Run()
	}
	if err := removeManagedSystemdOverride(unitPath, executable); err != nil {
		return err
	}
	if unitManaged {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	return removeManagedDesktopFile(filepath.Join(config, "autostart", "replicaro.desktop"))
}

var vendorSystemdUnitPaths = []string{
	"/usr/lib/systemd/user/replicaro.service",
	"/usr/share/systemd/user/replicaro.service",
	"/lib/systemd/user/replicaro.service",
}

func hasVendorSystemdUnit() bool {
	for _, path := range vendorSystemdUnitPaths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func managedSystemdUnit(executable string, marker bool) string {
	prefix := ""
	if marker {
		prefix = "# Managed by Replicaro\n"
	}
	return prefix + "[Unit]\nDescription=Replicaro backup companion\nAfter=network-online.target\n\n[Service]\nType=simple\nExecStart=" + systemdEscape(executable) + "\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n"
}

func removeManagedSystemdOverride(path, executable string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	content := string(data)
	managed := strings.HasPrefix(content, "# Managed by Replicaro\n") ||
		(executable != "" && content == managedSystemdUnit(executable, false))
	if !managed {
		return nil
	}
	return os.Remove(path)
}

func writeStartupFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".replicaro-startup-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func writeManagedStartupFile(path string, data []byte, mode os.FileMode, legacy string) error {
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != legacy && !strings.HasPrefix(string(existing), "# Managed by Replicaro\n") {
			return errors.New("refusing to replace an unmanaged startup file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeStartupFile(path, data, mode)
}

const launchAgentManagedMarker = "<!-- Managed by Replicaro -->\n"
const launchAgentManagedKey = `<key>io.replicaro.managed</key><true/>`
const launchAgentManagedPrefix = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
` + launchAgentManagedMarker + `<plist version="1.0"><dict>` + launchAgentManagedKey

func managedLaunchAgentContent(executable string, marker bool) string {
	markerText := ""
	markerField := ""
	if marker {
		markerText = launchAgentManagedMarker
		markerField = launchAgentManagedKey
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
` + markerText + `<plist version="1.0"><dict>` + markerField + `<key>Label</key><string>io.replicaro.desktop</string><key>ProgramArguments</key><array><string>` + xmlEscape(executable) + `</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/></dict></plist>
`
}

func launchAgentContentManaged(content, executable string) bool {
	return strings.HasPrefix(content, launchAgentManagedPrefix) || content == managedLaunchAgentContent(executable, false)
}

func writeManagedLaunchAgentFile(path string, data, legacy []byte) error {
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(legacy) && !launchAgentContentManaged(string(existing), "") {
			return errors.New("refusing to replace an unmanaged LaunchAgent")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeStartupFile(path, data, 0o600)
}

func removeManagedLaunchAgentFile(path, executable string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !launchAgentContentManaged(string(data), executable) {
		return nil
	}
	return os.Remove(path)
}

func removeManagedDesktopFile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(data), "# Managed by Replicaro\n") {
		return nil
	}
	return os.Remove(path)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}
func systemdEscape(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(value) + `"`
}
func desktopEscape(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `%`, `%%`).Replace(value) + `"`
}
