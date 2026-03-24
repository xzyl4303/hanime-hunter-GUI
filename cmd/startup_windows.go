//go:build windows

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lxn/win"
	"github.com/spf13/cobra"
)

func handleDirectStartup() (bool, error) {
	if len(os.Args) > 1 {
		return false, nil
	}
	if !isGUIExecutable() {
		return false, nil
	}
	return true, runGUI()
}

func prepareStartupCommand(cmd *cobra.Command) {
	if cmd == nil {
		return
	}

	if len(os.Args) > 1 {
		return
	}

	if !isGUIExecutable() {
		return
	}

	cmd.SetArgs([]string{"gui"})
}

func reportStartupError(err error) {
	if err == nil {
		return
	}

	if !isGUIExecutable() {
		return
	}

	title, titleErr := syscall.UTF16PtrFromString(guiWindowTitle)
	msg, msgErr := syscall.UTF16PtrFromString(err.Error())
	if titleErr != nil || msgErr != nil {
		return
	}

	win.MessageBox(0, msg, title, win.MB_OK|win.MB_ICONERROR)
}

func isGUIExecutable() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}

	base := strings.ToLower(filepath.Base(exe))
	return strings.HasSuffix(base, "-gui.exe")
}
