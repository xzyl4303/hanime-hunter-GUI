//go:build !windows

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var guiCmd = &cobra.Command{
	Use:    "gui",
	Short:  "启动原生图形界面",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("原生 GUI 当前仅支持 Windows")
	},
}

func runDefaultCommand(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}
