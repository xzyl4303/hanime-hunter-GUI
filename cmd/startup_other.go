//go:build !windows

package cmd

import "github.com/spf13/cobra"

func handleDirectStartup() (bool, error) { return false, nil }

func prepareStartupCommand(cmd *cobra.Command) {}

func reportStartupError(err error) {}
