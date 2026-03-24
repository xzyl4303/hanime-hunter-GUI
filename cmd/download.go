package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/acgtools/hanime-hunter/internal/downloader"
	"github.com/acgtools/hanime-hunter/internal/resolvers"
	"github.com/acgtools/hanime-hunter/internal/tui/progressbar"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/sync/semaphore"
)

const (
	defaultRetries = 10
	defaultThreads = 20
	maxThreads     = 64

	progressJSONEnv     = "HANI_PROGRESS_JSON"
	progressEventPrefix = "[HANI_PROGRESS] "
)

type progressLine struct {
	File       string  `json:"file,omitempty"`
	Ratio      float64 `json:"ratio,omitempty"`
	Percent    int     `json:"percent,omitempty"`
	Status     string  `json:"status,omitempty"`
	Speed      int64   `json:"speed,omitempty"`
	Remaining  int64   `json:"remainingSec,omitempty"`
	Downloaded int64   `json:"downloaded,omitempty"`
	Total      int64   `json:"total,omitempty"`
}

type progressEmitter struct {
	enabled bool

	mu   sync.Mutex
	last map[string]time.Time
}

func newProgressEmitter(enabled bool) *progressEmitter {
	return &progressEmitter{
		enabled: enabled,
		last:    make(map[string]time.Time),
	}
}

func (e *progressEmitter) Emit(evt downloader.ProgressEvent) {
	if e == nil || !e.enabled {
		return
	}

	ratio := evt.Ratio
	switch {
	case ratio < 0:
		ratio = 0
	case ratio > 1:
		ratio = 1
	}

	fileKey := evt.FileName
	if fileKey == "" {
		fileKey = "_task"
	}

	now := time.Now()
	shouldForce := evt.Status != "" || ratio >= 1

	e.mu.Lock()
	last := e.last[fileKey]
	if !shouldForce && !last.IsZero() && now.Sub(last) < 120*time.Millisecond {
		e.mu.Unlock()
		return
	}
	e.last[fileKey] = now
	e.mu.Unlock()

	line := progressLine{
		File:       evt.FileName,
		Ratio:      ratio,
		Percent:    int(ratio*100 + 0.5),
		Status:     evt.Status,
		Speed:      evt.Speed,
		Remaining:  int64(evt.RemainingS + 0.5),
		Downloaded: evt.Downloaded,
		Total:      evt.Total,
	}

	data, err := json.Marshal(line)
	if err != nil {
		return
	}

	fmt.Println(progressEventPrefix + string(data))
}

var dlCmd = &cobra.Command{
	Use:   "dl",
	Short: "下载视频",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := NewCfg()
		if err != nil {
			return err
		}
		if cfg.DLOpt.Threads == 0 {
			cfg.DLOpt.Threads = defaultThreads
		}
		if cfg.DLOpt.Threads > maxThreads {
			return fmt.Errorf("线程数超出范围，最大支持 %d", maxThreads)
		}

		logLevel, err := log.ParseLevel(cfg.Log.Level)
		if err != nil {
			return fmt.Errorf("解析日志级别失败: %w", err)
		}
		log.SetLevel(logLevel)
		log.SetReportTimestamp(false)

		return download(args[0], cfg)
	},
}

func download(aniURL string, cfg *Config) error {
	anis, err := resolvers.Resolve(aniURL, &resolvers.Option{
		Series: cfg.ResolverOpt.Series,
	})
	if err != nil {
		return err //nolint:wrapcheck
	}

	progressJSONEnabled := os.Getenv(progressJSONEnv) == "1"
	emitter := newProgressEmitter(progressJSONEnabled)

	var (
		m *progressbar.Model
		p *tea.Program
	)
	if !progressJSONEnabled {
		m = &progressbar.Model{
			Mux: sync.Mutex{},
			Pbs: make(map[string]*progressbar.ProgressBar),
		}
		p = tea.NewProgram(m)
	}

	d := downloader.NewDownloader(p, &downloader.Option{
		OutputDir:  cfg.DLOpt.OutputDir,
		Quality:    cfg.DLOpt.Quality,
		Info:       cfg.DLOpt.Info,
		LowQuality: cfg.DLOpt.LowQuality,
		Retry:      cfg.DLOpt.Retry,
		Threads:    cfg.DLOpt.Threads,
		ProgressCallback: func(evt downloader.ProgressEvent) {
			emitter.Emit(evt)
		},
	})

	if d.Option.Info {
		log.Infof("开始获取视频信息...")
	} else {
		log.Info("开始下载...")
	}

	ctx := context.Background()
	sem := semaphore.NewWeighted(int64(runtime.GOMAXPROCS(0)))
	wg := sync.WaitGroup{}
	errs := make([]error, len(anis))
	for i, ani := range anis {
		wg.Add(1)

		go func(idx int, a *resolvers.HAnime) {
			defer wg.Done()

			if err := sem.Acquire(ctx, 1); err != nil {
				log.Errorf("获取并发信号量失败: %v", err)
				return
			}
			defer sem.Release(1)

			if err := d.Download(a, m); err != nil {
				errs[idx] = err
			}
		}(i, ani)
	}

	if p != nil {
		go func() {
			wg.Wait()
			p.Send(progressbar.ProgressCompleteMsg{})
		}()

		if _, err := p.Run(); err != nil {
			return err //nolint:wrapcheck
		}
	} else {
		wg.Wait()
	}

	for _, e := range errs {
		if e != nil {
			log.Errorf("下载失败: %v", e)
		}
	}

	return nil
}

func init() {
	// DL Opts
	dlCmd.Flags().StringP("output-dir", "o", "", "输出目录")
	dlCmd.Flags().StringP("quality", "q", "", "指定视频清晰度，例如：1080p、720p、480p")
	dlCmd.Flags().BoolP("info", "i", false, "仅获取可下载视频信息")
	dlCmd.Flags().Bool("low-quality", false, "下载最低清晰度视频")
	dlCmd.Flags().Uint8("retry", defaultRetries, "重试次数，最大 255")
	dlCmd.Flags().Uint8("threads", defaultThreads, "下载线程数，范围 1-64")

	_ = viper.BindPFlag("DLOpt.OutputDir", dlCmd.Flags().Lookup("output-dir"))
	_ = viper.BindPFlag("DLOpt.Quality", dlCmd.Flags().Lookup("quality"))
	_ = viper.BindPFlag("DLOpt.Info", dlCmd.Flags().Lookup("info"))
	_ = viper.BindPFlag("DLOpt.LowQuality", dlCmd.Flags().Lookup("low-quality"))
	_ = viper.BindPFlag("DLOpt.Retry", dlCmd.Flags().Lookup("retry"))
	_ = viper.BindPFlag("DLOpt.Threads", dlCmd.Flags().Lookup("threads"))

	// Resolver Opts
	dlCmd.Flags().BoolP("series", "s", false, "下载整季/全集")

	_ = viper.BindPFlag("ResolverOpt.Series", dlCmd.Flags().Lookup("series"))
}
