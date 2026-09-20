package jobscript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/local/replicaro/command"
)

// ValidatePath admits only an exact absolute existing regular file. On POSIX
// the stored path itself must be executable so the kernel and shebang retain
// their native semantics.
func ValidatePath(path string) error {
	if path == "" {
		return nil
	}
	if strings.TrimSpace(path) != path || !filepath.IsAbs(path) {
		return fmt.Errorf("script path must be an exact absolute path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("script path must name an existing file")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("script path must name a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("POSIX script path must be executable and use a native executable or shebang")
	}
	if runtime.GOOS == "windows" {
		extension := strings.ToLower(filepath.Ext(path))
		if extension != ".bat" && extension != ".cmd" && extension != ".ps1" && extension != ".exe" && extension != ".com" {
			return fmt.Errorf("Windows script path must be a batch, PowerShell, or executable file")
		}
		if (extension == ".bat" || extension == ".cmd") && strings.ContainsAny(path, "&|<>^%!\"") {
			return fmt.Errorf("Windows batch script path contains characters unsafe for fixed cmd.exe invocation")
		}
	}
	return nil
}

func ValidateDefinition(before string, beforeRequired bool, after string, afterRequired bool) error {
	if before == "" && beforeRequired {
		return fmt.Errorf("before-script required checkbox needs a script path")
	}
	if after == "" && afterRequired {
		return fmt.Errorf("after-script required checkbox needs a script path")
	}
	if err := ValidatePath(before); err != nil {
		return fmt.Errorf("before script: %w", err)
	}
	if err := ValidatePath(after); err != nil {
		return fmt.Errorf("after script: %w", err)
	}
	return nil
}

// Run executes one validated stored path with fixed platform handling. It
// supplies no arguments or environment overrides and never returns script
// output.
func Run(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" {
		return errors.New("script file is missing or unusable")
	}
	if err := ValidatePath(path); err != nil {
		return errors.New("script file is missing or unusable")
	}
	executable := path
	args := []string{}
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".bat", ".cmd":
			systemRoot := os.Getenv("SystemRoot")
			if systemRoot == "" {
				return errors.New("script launch failed")
			}
			executable = filepath.Join(systemRoot, "System32", "cmd.exe")
			// Keep the fixed command verb outside the quoted path. cmd.exe's /S
			// handling otherwise removes the only quote pair around a path with
			// spaces before it resolves the batch file.
			args = []string{"/D", "/S", "/C", "call", path}
		case ".ps1":
			systemRoot := os.Getenv("SystemRoot")
			if systemRoot == "" {
				return errors.New("script launch failed")
			}
			executable = filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
			args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-File", path}
		}
	}
	err := command.RunDiscardOutput(command.ContextWithFinalCancellationAdmission(ctx), executable, args, nil, "job script")
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return errors.New("script completed unsuccessfully")
	}
	return errors.New("script launch failed")
}
