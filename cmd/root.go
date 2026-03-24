package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rootCmd = &cobra.Command{
	Use:   "hani",
	Short: "HAnime downloader",
	Long:  "HAnime downloader. Repository: https://github.com/acgtools/hanime-hunter",
	RunE:  runDefaultCommand,
}

func Execute() {
	if handled, err := handleDirectStartup(); handled {
		if err != nil {
			reportStartupError(err)
			os.Exit(1)
		}
		return
	}

	rootCmd.CompletionOptions.DisableDefaultCmd = true
	prepareStartupCommand(rootCmd)

	err := rootCmd.Execute()
	if err != nil {
		reportStartupError(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().String("log-level", "info", "日志级别，可选：debug、info、warn、error、fatal")

	_ = viper.BindPFlag("log.level", rootCmd.PersistentFlags().Lookup("log-level"))

	rootCmd.AddCommand(verCmd, dlCmd, guiCmd)
}
