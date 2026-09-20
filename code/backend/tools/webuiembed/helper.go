package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// prepareHelper uses the same first-party compiler inputs as the unsigned
// compile stages. The output belongs to this invocation, never to the source
// tree. Windows/macOS signed drivers apply native signatures before computing
// the checksum and final overlay; Linux release signatures are detached.
func prepareHelper(target, outputRoot string) error {
	switch target {
	case "windows_amd64", "linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64":
	default:
		return fmt.Errorf("unsupported storage-helper target: %s", target)
	}
	backend, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(backend, "cmd", "storage-helper", "main.go")); err != nil {
		return fmt.Errorf("run helper preparation from code/backend: %w", err)
	}
	out, err := filepath.Abs(outputRoot)
	if err != nil {
		return err
	}
	if err := validateExistingPathComponents(out); err != nil {
		return err
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		return fmt.Errorf("helper preparation requires a fresh output root: %s", out)
	}
	// Explicit target verification precedes output creation and native-byte use.
	verify := exec.Command("go", "run", "-mod=readonly", "./tools/targetverify", "--target", target)
	verify.Stdout, verify.Stderr = os.Stdout, os.Stderr
	if err := verify.Run(); err != nil {
		return fmt.Errorf("verify selected target before helper preparation: %w", err)
	}
	name := "storage-helper"
	if target == "windows_amd64" {
		name += ".exe"
	}
	built := filepath.Join(out, "embed", "storagehelper", "assets", target, name)
	if err := os.MkdirAll(filepath.Dir(built), 0o700); err != nil {
		return err
	}
	parts := strings.Split(target, "_")
	build := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", built, "./cmd/storage-helper")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+parts[0], "GOARCH="+parts[1])
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("compile selected-target storage helper: %w", err)
	}
	data, err := os.ReadFile(built)
	if err != nil {
		return err
	}
	if err := os.WriteFile(built+".sha256", []byte(fmt.Sprintf("%x\n", sha256.Sum256(data))), 0o600); err != nil {
		return err
	}
	source := filepath.Join(backend, "storagehelper", "assets", target, name)
	overlay := overlayFile{Replace: map[string]string{source: built, source + ".sha256": built + ".sha256"}}
	if err := validateHelperOverlay(overlay, source, built); err != nil {
		return err
	}
	return writeOverlay(filepath.Join(out, "storage-helper-overlay.json"), overlay)
}
