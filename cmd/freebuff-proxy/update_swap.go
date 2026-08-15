package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// replaceExecutable installs the freshly downloaded binary (tempPath, in the
// same directory as execPath) over the currently running executable.
// On Unix the swap is atomic (rename); on Windows the running executable
// cannot be replaced while in use, so a detached helper script performs the
// swap after this process exits. The returned message describes deferred
// behavior when applicable.
func replaceExecutable(execPath, tempPath string) (string, error) {
	if runtime.GOOS == "windows" {
		return installWindows(execPath, tempPath)
	}
	if err := installUnix(execPath, tempPath); err != nil {
		return "", err
	}
	return "", nil
}

// installUnix atomically replaces execPath with tempPath via rename: the old
// binary is moved aside to execPath.old, the new one is renamed into place,
// and the old file is removed.
func installUnix(execPath, tempPath string) error {
	oldPath := execPath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("rename current binary aside: %w", err)
	}
	if err := os.Rename(tempPath, execPath); err != nil {
		_ = os.Rename(oldPath, execPath) // rollback
		return fmt.Errorf("install updated binary: %w", err)
	}
	_ = os.Remove(oldPath)
	return nil
}

// installWindows writes a small .bat helper next to the executable that waits
// for the current process (pid) to exit, then moves the downloaded temp file
// into place and deletes itself. The helper is launched detached via
// `cmd /c start`, so it survives this process exiting.
func installWindows(execPath, tempPath string) (string, error) {
	batPath := execPath + ".update.bat"
	script := windowsUpdateScript(execPath, tempPath, os.Getpid())
	if err := os.WriteFile(batPath, []byte(script), 0755); err != nil {
		return "", fmt.Errorf("write update helper script: %w", err)
	}

	cmd := exec.Command("cmd", "/c", "start", "", batPath)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(batPath)
		return "", fmt.Errorf("launch update helper script: %w", err)
	}

	return fmt.Sprintf("The new binary will be installed automatically after this process (PID %d) exits.", os.Getpid()), nil
}

// windowsUpdateScript returns the content of the .bat helper used to install
// the updated binary on Windows. It polls tasklist until the updating process
// (pid) is gone, moves tempPath over execPath, and then deletes itself.
func windowsUpdateScript(execPath, tempPath string, pid int) string {
	return fmt.Sprintf(`@echo off
setlocal
set "TARGET_PID=%d"
set "TEMP_FILE=%s"
set "EXE_FILE=%s"

:waitloop
tasklist /FI "PID eq %%TARGET_PID%%" 2>nul | findstr "%%TARGET_PID%%" >nul
if errorlevel 1 goto install
timeout /t 1 /nobreak >nul
goto waitloop

:install
move /y "%%TEMP_FILE%%" "%%EXE_FILE%%" >nul
if errorlevel 1 echo ERROR: failed to install updated binary: %%EXE_FILE%% > "%%EXE_FILE%%.update.log"
del "%%~f0"
endlocal
`, pid, tempPath, execPath)
}
