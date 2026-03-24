package cmd

import (
	"fmt"
	"runtime"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
)

var version = "unknown"

var verCmd = &cobra.Command{
	Use:   "version",
	Short: "显示版本信息",
	Run: func(cmd *cobra.Command, args []string) {
		goVersion := runtime.Version()
		platform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)

		log.SetTimeFunction(func() time.Time {
			return time.Time{}
		})
		log.Printf("版本：%s，Go：%s，平台：%s", version, goVersion, platform)
	},
}
