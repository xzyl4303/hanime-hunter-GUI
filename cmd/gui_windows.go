//go:build windows

package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/charmbracelet/log"
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"github.com/spf13/cobra"
)

const (
	guiWindowTitle      = "HAnime Hunter"
	defaultGUIMaxJob    = 2
	maxGUIMaxJob        = 16
	guiRefreshInterval  = 120 * time.Millisecond
	guiStatusMinRefresh = 800 * time.Millisecond
	guiSettingsVersion  = 2
)

var (
	guiWorkDir string
	guiMaxJobs int

	qualityChoices  = []string{"自动选择", "1080p", "720p", "480p", "360p", "240p"}
	logLevelChoices = []string{"info", "debug", "warn", "error", "fatal"}

	guiColorWindow     = walk.RGB(244, 247, 251)
	guiColorCard       = walk.RGB(255, 255, 255)
	guiColorAccent     = walk.RGB(36, 96, 222)
	guiColorAccentSoft = walk.RGB(235, 242, 255)
	guiColorText       = walk.RGB(26, 34, 48)
	guiColorMuted      = walk.RGB(106, 116, 132)
	guiColorBorder     = walk.RGB(224, 230, 238)
)

type desktopTaskStatus string

const (
	desktopTaskQueued   desktopTaskStatus = "queued"
	desktopTaskRunning  desktopTaskStatus = "running"
	desktopTaskDone     desktopTaskStatus = "done"
	desktopTaskError    desktopTaskStatus = "error"
	desktopTaskCanceled desktopTaskStatus = "canceled"
)

const (
	sideTabPreview = iota
	sideTabDetails
	sideTabLogs
	sideTabSettings
)

type guiSettings struct {
	Version          int    `json:"version"`
	MaxConcurrent    int    `json:"maxConcurrent"`
	OutputDir        string `json:"outputDir"`
	WorkDir          string `json:"workDir"`
	Quality          string `json:"quality"`
	Retry            uint8  `json:"retry"`
	Threads          uint8  `json:"threads"`
	TimeoutSec       int    `json:"timeoutSec"`
	Info             bool   `json:"info"`
	Series           bool   `json:"series"`
	LowQuality       bool   `json:"lowQuality"`
	LogLevel         string `json:"logLevel"`
	AutoPreview      bool   `json:"autoPreview"`
	AutoOpenOutput   bool   `json:"autoOpenOutput"`
	AutoSelectNewest bool   `json:"autoSelectNewest"`
	FocusLogsOnError bool   `json:"focusLogsOnError"`
}

type desktopDownloadRequest struct {
	URL        string
	OutputDir  string
	WorkDir    string
	Quality    string
	Retry      uint8
	Threads    uint8
	TimeoutSec int
	Info       bool
	LowQuality bool
	Series     bool
	LogLevel   string
}

type desktopTask struct {
	id int64

	req        desktopDownloadRequest
	createdAt  time.Time
	startedAt  time.Time
	finishedAt time.Time

	status   desktopTaskStatus
	errMsg   string
	exitCode int
	logs     []string
	logVer   int64

	progressRatio      float64
	progressStatus     string
	progressFile       string
	progressSpeed      int64
	progressRemainingS int64
	fileProgress       map[string]float64

	ctx    context.Context
	cancel context.CancelFunc

	mu sync.RWMutex
}

type desktopTaskSnapshot struct {
	ID         int64
	URL        string
	OutputDir  string
	WorkDir    string
	Quality    string
	Retry      uint8
	Threads    uint8
	TimeoutSec int
	Info       bool
	LowQuality bool
	Series     bool
	LogLevel   string
	Status     string
	Error      string
	ExitCode   int
	Progress   float64
	Percent    int
	PStage     string
	PFile      string
	PSpeed     int64
	PRemainS   int64
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Logs       []string
	LogVer     int64
}

type desktopTaskTableModel struct {
	walk.TableModelBase
	items []*desktopTask
}

func (m *desktopTaskTableModel) RowCount() int {
	return len(m.items)
}

func (m *desktopTaskTableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}

	snap := m.items[row].snapshot(false)

	switch col {
	case 0:
		return snap.ID
	case 1:
		return desktopStatusLabel(snap.Status)
	case 2:
		return fmt.Sprintf("%d%%", snap.Percent)
	case 3:
		return trimDisplay(taskPrimaryText(snap), 42)
	case 4:
		return displayQuality(snap.Quality)
	case 5:
		return formatSpeed(snap.PSpeed)
	case 6:
		return trimDisplay(snap.Error, 48)
	default:
		return ""
	}
}

func (m *desktopTaskTableModel) setTasks(tasks []*desktopTask) {
	m.items = append(m.items[:0], tasks...)
	m.PublishRowsReset()
}

func (m *desktopTaskTableModel) prependTask(task *desktopTask) {
	m.items = append([]*desktopTask{task}, m.items...)
	m.PublishRowsReset()
}

func (m *desktopTaskTableModel) indexOf(taskID int64) int {
	for i, task := range m.items {
		if task != nil && task.id == taskID {
			return i
		}
	}
	return -1
}

func (m *desktopTaskTableModel) taskAt(row int) *desktopTask {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	return m.items[row]
}

type desktopGUI struct {
	executable string
	workDir    string
	cfgPath    string
	maxJobs    int

	settings guiSettings

	mu    sync.RWMutex
	tasks map[int64]*desktopTask
	next  atomic.Int64

	runMu       sync.Mutex
	runningJobs int

	closed     atomic.Bool
	previewSeq atomic.Int64

	mw        *walk.MainWindow
	taskView  *walk.TableView
	taskModel *desktopTaskTableModel
	sideTabs  *walk.TabWidget

	refreshMu        sync.Mutex
	dirtyTaskIDs     map[int64]struct{}
	refreshScheduled bool
	lastStatusAt     time.Time

	logViewTaskID   int64
	logViewLogCount int
	logViewLogVer   int64

	autoPreviewMu    sync.Mutex
	autoPreviewTimer *time.Timer

	urlEdit         *walk.LineEdit
	outputDirEdit   *walk.LineEdit
	workDirEdit     *walk.LineEdit
	qualityCombo    *walk.ComboBox
	retryEdit       *walk.NumberEdit
	threadsEdit     *walk.NumberEdit
	timeoutEdit     *walk.NumberEdit
	logLevelCombo   *walk.ComboBox
	infoCheck       *walk.CheckBox
	seriesCheck     *walk.CheckBox
	lowQualityCheck *walk.CheckBox

	startBtn        *walk.PushButton
	previewBtn      *walk.PushButton
	cancelBtn       *walk.PushButton
	retryBtn        *walk.PushButton
	clearBtn        *walk.PushButton
	openOutputBtn   *walk.PushButton
	browseOutputBtn *walk.PushButton
	browseWorkBtn   *walk.PushButton

	previewImage       *walk.ImageView
	previewInfoEdit    *walk.TextLabel
	previewStatusLabel *walk.TextLabel
	previewBitmap      *walk.Bitmap

	progressBar   *walk.ProgressBar
	progressLabel *walk.TextLabel
	summaryEdit   *walk.TextEdit
	logEdit       *walk.TextEdit
	statusLabel   *walk.TextLabel

	settingsOutputDirEdit    *walk.LineEdit
	settingsWorkDirEdit      *walk.LineEdit
	settingsQualityCombo     *walk.ComboBox
	settingsRetryEdit        *walk.NumberEdit
	settingsThreadsEdit      *walk.NumberEdit
	settingsTimeoutEdit      *walk.NumberEdit
	settingsMaxJobsEdit      *walk.NumberEdit
	settingsLogLevelCombo    *walk.ComboBox
	settingsInfoCheck        *walk.CheckBox
	settingsSeriesCheck      *walk.CheckBox
	settingsLowQualityCheck  *walk.CheckBox
	settingsAutoPreviewCheck *walk.CheckBox
	settingsAutoOpenCheck    *walk.CheckBox
	settingsAutoSelectCheck  *walk.CheckBox
	settingsFocusLogsCheck   *walk.CheckBox
	settingsStatusLabel      *walk.TextLabel
	settingsBrowseOutputBtn  *walk.PushButton
	settingsBrowseWorkBtn    *walk.PushButton

	previewMu sync.RWMutex
	preview   *videoPreview
}

var guiCmd = &cobra.Command{
	Use:   "gui",
	Short: "启动原生图形界面",
	Long:  "启动 Windows 原生桌面界面，直接管理下载任务。",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGUI()
	},
}

func runDefaultCommand(cmd *cobra.Command, args []string) error {
	return runGUI()
}

func runGUI() error {
	focused, err := focusExistingGUIWindow()
	if err != nil {
		log.Warnf("focus existing GUI window failed: %v", err)
	} else if focused {
		return nil
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	workDir := strings.TrimSpace(guiWorkDir)
	if workDir == "" {
		workDir, err = os.Getwd()
		if err != nil {
			workDir = filepath.Dir(executable)
		}
	}

	cfgPath, err := defaultGUISettingsPath()
	if err != nil {
		return fmt.Errorf("resolve GUI settings path: %w", err)
	}

	settings, err := loadGUISettings(cfgPath)
	if err != nil {
		log.Warnf("load GUI settings failed: %v", err)
	}
	settings = normalizeGUISettings(settings, workDir)

	maxJobs := settings.MaxConcurrent
	if guiMaxJobs > 0 {
		maxJobs = guiMaxJobs
	}
	if maxJobs < 1 {
		maxJobs = defaultGUIMaxJob
	}

	app := &desktopGUI{
		executable:   executable,
		workDir:      workDir,
		cfgPath:      cfgPath,
		maxJobs:      maxJobs,
		settings:     settings,
		tasks:        make(map[int64]*desktopTask),
		taskModel:    &desktopTaskTableModel{},
		dirtyTaskIDs: make(map[int64]struct{}),
	}

	if err := app.createWindow(); err != nil {
		return err
	}

	app.applySettingsToForm(settings)
	app.applyDefaultPreviewState()
	app.refreshDetails(nil)
	app.updateButtons()
	app.setStatus(fmt.Sprintf("原生 GUI 已启动，最多同时运行 %d 个任务。", maxJobs))

	app.mw.Show()
	app.mw.Run()
	return nil
}

func focusExistingGUIWindow() (bool, error) {
	title, err := syscall.UTF16PtrFromString(guiWindowTitle)
	if err != nil {
		return false, err
	}

	hwnd := win.FindWindow(nil, title)
	if hwnd == 0 {
		return false, nil
	}

	if !win.IsWindowVisible(hwnd) {
		win.ShowWindow(hwnd, win.SW_RESTORE)
		win.ShowWindow(hwnd, win.SW_SHOW)
	}

	ensureWindowVisible(hwnd)

	if win.IsIconic(hwnd) {
		win.ShowWindow(hwnd, win.SW_RESTORE)
	} else {
		win.ShowWindow(hwnd, win.SW_SHOW)
	}
	win.SetWindowPos(hwnd, win.HWND_TOPMOST, 0, 0, 0, 0, win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_SHOWWINDOW)
	win.SetWindowPos(hwnd, win.HWND_NOTOPMOST, 0, 0, 0, 0, win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_SHOWWINDOW)
	win.BringWindowToTop(hwnd)
	win.SetForegroundWindow(hwnd)
	return true, nil
}

func ensureWindowVisible(hwnd win.HWND) {
	if hwnd == 0 {
		return
	}

	monitor := win.MonitorFromWindow(hwnd, win.MONITOR_DEFAULTTONEAREST)
	if monitor == 0 {
		return
	}

	info := win.MONITORINFO{CbSize: uint32(unsafe.Sizeof(win.MONITORINFO{}))}
	if !win.GetMonitorInfo(monitor, &info) {
		return
	}

	var rect win.RECT
	if !win.GetWindowRect(hwnd, &rect) {
		return
	}

	work := info.RcWork
	width := rect.Right - rect.Left
	height := rect.Bottom - rect.Top
	if width <= 0 || height <= 0 {
		return
	}

	left := rect.Left
	top := rect.Top
	if left < work.Left || top < work.Top || rect.Right > work.Right || rect.Bottom > work.Bottom {
		if width > work.Right-work.Left {
			width = work.Right - work.Left
		}
		if height > work.Bottom-work.Top {
			height = work.Bottom - work.Top
		}
		left = work.Left + ((work.Right-work.Left)-width)/2
		top = work.Top + ((work.Bottom-work.Top)-height)/2
		win.MoveWindow(hwnd, left, top, width, height, true)
	}
}

func (app *desktopGUI) fitWindowToScreen() {
	if app.mw == nil {
		return
	}

	monitor := win.MonitorFromWindow(app.mw.Handle(), win.MONITOR_DEFAULTTONEAREST)
	if monitor == 0 {
		return
	}

	info := win.MONITORINFO{CbSize: uint32(unsafe.Sizeof(win.MONITORINFO{}))}
	if !win.GetMonitorInfo(monitor, &info) {
		return
	}

	workWidth := int(info.RcWork.Right - info.RcWork.Left)
	workHeight := int(info.RcWork.Bottom - info.RcWork.Top)
	if workWidth <= 0 || workHeight <= 0 {
		return
	}

	width := int(float64(workWidth) * 0.9)
	height := int(float64(workHeight) * 0.88)
	if width < 960 {
		width = workWidth - 24
	}
	if height < 680 {
		height = workHeight - 24
	}
	if width > workWidth-12 {
		width = workWidth - 12
	}
	if height > workHeight-12 {
		height = workHeight - 12
	}
	if width <= 0 || height <= 0 {
		return
	}

	x := int(info.RcWork.Left) + (workWidth-width)/2
	y := int(info.RcWork.Top) + (workHeight-height)/2
	_ = app.mw.SetBoundsPixels(walk.Rectangle{X: x, Y: y, Width: width, Height: height})
}

func (app *desktopGUI) createWindow() error {
	if err := (MainWindow{
		AssignTo:   &app.mw,
		Title:      guiWindowTitle,
		MinSize:    Size{960, 680},
		Size:       Size{1460, 920},
		Font:       Font{Family: "Microsoft YaHei UI", PointSize: 10},
		Background: SolidColorBrush{Color: guiColorWindow},
		Layout:     VBox{Margins: Margins{16, 16, 16, 16}, Spacing: 14},
		Children: []Widget{
			app.guiHeaderWidget(),
			HSplitter{
				HandleWidth: 10,
				Children: []Widget{
					VSplitter{
						HandleWidth:   10,
						StretchFactor: 3,
						MinSize:       Size{560, 0},
						Children: []Widget{
							app.downloadFormWidget(),
							app.taskQueueWidget(),
						},
					},
					app.sideWorkspaceWidget(),
				},
			},
		},
	}).Create(); err != nil {
		return err
	}

	app.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		app.closed.Store(true)
		app.stopAutoPreviewTimer()
		app.cancelAllTasks()
	})

	app.fitWindowToScreen()

	_ = app.retryEdit.SetRange(0, 255)
	_ = app.threadsEdit.SetRange(1, maxThreads)
	_ = app.timeoutEdit.SetRange(0, 86400)
	_ = app.settingsRetryEdit.SetRange(0, 255)
	_ = app.settingsThreadsEdit.SetRange(1, maxThreads)
	_ = app.settingsTimeoutEdit.SetRange(0, 86400)
	_ = app.settingsMaxJobsEdit.SetRange(1, maxGUIMaxJob)

	return nil
}

func (app *desktopGUI) guiHeaderWidget() Widget {
	return Composite{
		Background: SolidColorBrush{Color: guiColorAccent},
		Layout:     HBox{Margins: Margins{24, 20, 24, 20}, Spacing: 16},
		MinSize:    Size{0, 116},
		Children: []Widget{
			Composite{
				Layout: VBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{
						Text:      "HAnime Hunter",
						Font:      Font{Family: "Microsoft YaHei UI", PointSize: 19, Bold: true},
						TextColor: walk.RGB(255, 255, 255),
					},
					TextLabel{
						Text:      "原生桌面下载器。左侧专注任务与队列，右侧使用响应式标签页承载预览、详情、日志与设置。",
						TextColor: walk.RGB(232, 238, 248),
						MinSize:   Size{520, 0},
					},
				},
			},
			HSpacer{},
			Composite{
				Background: SolidColorBrush{Color: guiColorAccentSoft},
				Border:     true,
				MinSize:    Size{260, 0},
				Layout:     VBox{Margins: Margins{16, 14, 16, 14}, Spacing: 4},
				Children: []Widget{
					Label{
						Text:      "当前会话",
						Font:      Font{Family: "Microsoft YaHei UI", PointSize: 9, Bold: true},
						TextColor: guiColorAccent,
					},
					TextLabel{
						AssignTo:  &app.statusLabel,
						Text:      "准备就绪",
						TextColor: guiColorText,
						MinSize:   Size{260, 0},
					},
				},
			},
		},
	}
}

func (app *desktopGUI) downloadFormWidget() Widget {
	return Composite{
		Background:    SolidColorBrush{Color: guiColorCard},
		Border:        true,
		StretchFactor: 2,
		Layout:        VBox{Margins: Margins{18, 16, 18, 16}, Spacing: 12},
		Children: []Widget{
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 12},
				Children: []Widget{
					Composite{
						Layout: VBox{MarginsZero: true, Spacing: 4},
						Children: []Widget{
							Label{Text: "下载设置", Font: Font{Family: "Microsoft YaHei UI", PointSize: 12, Bold: true}, TextColor: guiColorText},
							TextLabel{Text: "当前任务参数集中在这里。默认参数和界面行为可在右侧“设置”页单独维护。", TextColor: guiColorMuted, MinSize: Size{520, 0}},
						},
					},
				},
			},
			Composite{Background: SolidColorBrush{Color: guiColorBorder}, MinSize: Size{0, 1}, MaxSize: Size{0, 1}},
			Composite{
				Layout: Grid{Columns: 8, Spacing: 10},
				Children: []Widget{
					Label{Text: "链接", Row: 0, Column: 0, TextColor: guiColorMuted},
					LineEdit{AssignTo: &app.urlEdit, Row: 0, Column: 1, ColumnSpan: 7, CueBanner: "粘贴 hanime1.me 或 hanime.tv 链接", OnTextChanged: app.onPreviewURLChanged},

					Label{Text: "输出目录", Row: 1, Column: 0, TextColor: guiColorMuted},
					LineEdit{AssignTo: &app.outputDirEdit, Row: 1, Column: 1, ColumnSpan: 5},
					PushButton{AssignTo: &app.browseOutputBtn, Text: "选择...", Row: 1, Column: 6, OnClicked: app.chooseOutputDir},
					PushButton{AssignTo: &app.openOutputBtn, Text: "打开目录", Row: 1, Column: 7, OnClicked: app.openCurrentOutputDir},

					Label{Text: "工作目录", Row: 2, Column: 0, TextColor: guiColorMuted},
					LineEdit{AssignTo: &app.workDirEdit, Row: 2, Column: 1, ColumnSpan: 5},
					PushButton{AssignTo: &app.browseWorkBtn, Text: "选择...", Row: 2, Column: 6, OnClicked: app.chooseWorkDir},
					TextLabel{Text: "下载过程默认使用程序目录，也可以按任务切换。", Row: 2, Column: 7, MinSize: Size{160, 0}, TextColor: guiColorMuted},

					Label{Text: "质量", Row: 3, Column: 0, TextColor: guiColorMuted},
					ComboBox{AssignTo: &app.qualityCombo, Editable: false, Model: qualityChoices, Row: 3, Column: 1},
					Label{Text: "重试", Row: 3, Column: 2, TextColor: guiColorMuted},
					NumberEdit{AssignTo: &app.retryEdit, Decimals: 0, Row: 3, Column: 3},
					Label{Text: "线程", Row: 3, Column: 4, TextColor: guiColorMuted},
					NumberEdit{AssignTo: &app.threadsEdit, Decimals: 0, Row: 3, Column: 5},
					Label{Text: "超时(秒)", Row: 3, Column: 6, TextColor: guiColorMuted},
					NumberEdit{AssignTo: &app.timeoutEdit, Decimals: 0, Row: 3, Column: 7},

					Label{Text: "日志级别", Row: 4, Column: 0, TextColor: guiColorMuted},
					ComboBox{AssignTo: &app.logLevelCombo, Editable: false, Model: logLevelChoices, Row: 4, Column: 1},
					CheckBox{AssignTo: &app.infoCheck, Text: "仅获取信息", Row: 4, Column: 2},
					CheckBox{AssignTo: &app.seriesCheck, Text: "整季/全集", Row: 4, Column: 3},
					CheckBox{AssignTo: &app.lowQualityCheck, Text: "最低质量", Row: 4, Column: 4},
					PushButton{AssignTo: &app.previewBtn, Text: "预览信息", Row: 4, Column: 5, OnClicked: app.previewCurrentURL},
					PushButton{AssignTo: &app.startBtn, Text: "开始下载", Row: 4, Column: 6, OnClicked: app.startTaskFromForm},
					PushButton{AssignTo: &app.cancelBtn, Text: "取消选中", Row: 4, Column: 7, OnClicked: app.cancelSelectedTask},
				},
			},
		},
	}
}

func (app *desktopGUI) taskQueueWidget() Widget {
	return Composite{
		Background:    SolidColorBrush{Color: guiColorCard},
		Border:        true,
		StretchFactor: 3,
		Layout:        VBox{Margins: Margins{18, 16, 18, 16}, Spacing: 12},
		Children: []Widget{
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 10},
				Children: []Widget{
					Composite{
						Layout: VBox{MarginsZero: true, Spacing: 4},
						Children: []Widget{
							Label{Text: "任务队列", Font: Font{Family: "Microsoft YaHei UI", PointSize: 12, Bold: true}, TextColor: guiColorText},
							TextLabel{Text: "列表区会随窗口伸缩。右侧标签页显示所选任务的预览、详情和日志。", TextColor: guiColorMuted, MinSize: Size{430, 0}},
						},
					},
					HSpacer{},
					PushButton{AssignTo: &app.retryBtn, Text: "重试选中任务", OnClicked: app.retrySelectedTask},
					PushButton{AssignTo: &app.clearBtn, Text: "清理已完成", OnClicked: app.clearFinishedTasks},
				},
			},
			Composite{Background: SolidColorBrush{Color: guiColorBorder}, MinSize: Size{0, 1}, MaxSize: Size{0, 1}},
			TableView{
				AssignTo:                    &app.taskView,
				Model:                       app.taskModel,
				AlternatingRowBG:            true,
				ColumnsOrderable:            true,
				ColumnsSizable:              true,
				LastColumnStretched:         true,
				NotSortableByHeaderClick:    true,
				SelectionHiddenWithoutFocus: false,
				MinSize:                     Size{0, 420},
				Columns: []TableViewColumn{
					{Title: "ID", Width: 56},
					{Title: "状态", Width: 78},
					{Title: "进度", Width: 72},
					{Title: "当前项", Width: 340},
					{Title: "质量", Width: 82},
					{Title: "速度", Width: 100},
					{Title: "错误", Width: 240},
				},
				OnCurrentIndexChanged: app.onTaskSelectionChanged,
				OnItemActivated:       app.onTaskSelectionChanged,
			},
		},
	}
}

func (app *desktopGUI) sideWorkspaceWidget() Widget {
	return Composite{
		StretchFactor: 2,
		MinSize:       Size{360, 0},
		Background:    SolidColorBrush{Color: guiColorWindow},
		Layout:        VBox{MarginsZero: true, Spacing: 0},
		Children: []Widget{
			Composite{
				Background: SolidColorBrush{Color: guiColorCard},
				Border:     true,
				Layout:     VBox{Margins: Margins{18, 16, 18, 16}, Spacing: 12},
				Children: []Widget{
					Composite{
						Layout: VBox{MarginsZero: true, Spacing: 4},
						Children: []Widget{
							Label{Text: "工作区", Font: Font{Family: "Microsoft YaHei UI", PointSize: 12, Bold: true}, TextColor: guiColorText},
							TextLabel{Text: "右侧采用响应式标签页，窗口收窄时也能稳定切换预览、详情、日志和设置。", TextColor: guiColorMuted, MinSize: Size{280, 0}},
						},
					},
					Composite{Background: SolidColorBrush{Color: guiColorBorder}, MinSize: Size{0, 1}, MaxSize: Size{0, 1}},
					TabWidget{
						AssignTo:       &app.sideTabs,
						StretchFactor:  1,
						ContentMargins: Margins{8, 8, 8, 8},
						Pages: []TabPage{
							{
								Title:  "预览",
								Layout: VBox{MarginsZero: true, Spacing: 12},
								Children: []Widget{
									TextLabel{Text: "支持先解析链接并查看缩略图、标题和清晰度。", TextColor: guiColorMuted, MinSize: Size{260, 0}},
									Composite{
										Background: SolidColorBrush{Color: guiColorAccentSoft},
										Border:     true,
										Layout:     VBox{Margins: Margins{12, 12, 12, 12}, Spacing: 12},
										Children: []Widget{
											ImageView{AssignTo: &app.previewImage, MinSize: Size{260, 156}, MaxSize: Size{420, 240}, Mode: ImageViewModeZoom, Margin: 10, Background: SolidColorBrush{Color: walk.RGB(255, 255, 255)}},
											TextLabel{AssignTo: &app.previewStatusLabel, Text: "输入链接后可预览缩略图与可用清晰度。", Font: Font{Family: "Microsoft YaHei UI", PointSize: 10, Bold: true}, TextColor: guiColorText, MinSize: Size{220, 0}},
											TextLabel{AssignTo: &app.previewInfoEdit, Text: "", TextColor: guiColorMuted, MinSize: Size{220, 160}},
										},
									},
								},
							},
							{
								Title:  "详情",
								Layout: VBox{MarginsZero: true, Spacing: 12},
								Children: []Widget{
									TextLabel{Text: "显示当前任务的阶段、速度、参数和错误信息。", TextColor: guiColorMuted, MinSize: Size{260, 0}},
									ProgressBar{AssignTo: &app.progressBar, MinValue: 0, MaxValue: 100, Value: 0},
									TextLabel{AssignTo: &app.progressLabel, Text: "未选择任务", TextColor: guiColorMuted, MinSize: Size{200, 0}},
									TextEdit{AssignTo: &app.summaryEdit, ReadOnly: true, VScroll: true, MinSize: Size{0, 220}},
								},
							},
							{
								Title:  "日志",
								Layout: VBox{MarginsZero: true, Spacing: 12},
								Children: []Widget{
									TextLabel{Text: "保留结构化日志，方便定位下载失败、超时和链接异常。", TextColor: guiColorMuted, MinSize: Size{260, 0}},
									TextEdit{AssignTo: &app.logEdit, ReadOnly: true, VScroll: true, MinSize: Size{0, 320}},
								},
							},
							{
								Title:  "设置",
								Layout: VBox{MarginsZero: true, Spacing: 12},
								Children: []Widget{
									Composite{
										Background: SolidColorBrush{Color: guiColorCard},
										Border:     true,
										Layout:     VBox{Margins: Margins{14, 14, 14, 14}, Spacing: 10},
										Children: []Widget{
											Label{Text: "默认下载参数", Font: Font{Family: "Microsoft YaHei UI", PointSize: 11, Bold: true}, TextColor: guiColorText},
											TextLabel{Text: "这里保存的是默认值。保存后会同步到左侧下载表单。", TextColor: guiColorMuted, MinSize: Size{260, 0}},
											Composite{Background: SolidColorBrush{Color: guiColorBorder}, MinSize: Size{0, 1}, MaxSize: Size{0, 1}},
											Composite{
												Layout: Grid{Columns: 8, Spacing: 10},
												Children: []Widget{
													Label{Text: "默认输出", Row: 0, Column: 0, TextColor: guiColorMuted},
													LineEdit{AssignTo: &app.settingsOutputDirEdit, Row: 0, Column: 1, ColumnSpan: 5},
													PushButton{AssignTo: &app.settingsBrowseOutputBtn, Text: "选择...", Row: 0, Column: 6, OnClicked: app.chooseSettingsOutputDir},
													TextLabel{Text: "作为新任务默认输出目录。", Row: 0, Column: 7, MinSize: Size{150, 0}, TextColor: guiColorMuted},

													Label{Text: "默认工作目录", Row: 1, Column: 0, TextColor: guiColorMuted},
													LineEdit{AssignTo: &app.settingsWorkDirEdit, Row: 1, Column: 1, ColumnSpan: 5},
													PushButton{AssignTo: &app.settingsBrowseWorkBtn, Text: "选择...", Row: 1, Column: 6, OnClicked: app.chooseSettingsWorkDir},
													TextLabel{Text: "CLI 子进程默认工作目录。", Row: 1, Column: 7, MinSize: Size{150, 0}, TextColor: guiColorMuted},

													Label{Text: "默认质量", Row: 2, Column: 0, TextColor: guiColorMuted},
													ComboBox{AssignTo: &app.settingsQualityCombo, Editable: false, Model: qualityChoices, Row: 2, Column: 1},
													Label{Text: "默认重试", Row: 2, Column: 2, TextColor: guiColorMuted},
													NumberEdit{AssignTo: &app.settingsRetryEdit, Decimals: 0, Row: 2, Column: 3},
													Label{Text: "默认线程", Row: 2, Column: 4, TextColor: guiColorMuted},
													NumberEdit{AssignTo: &app.settingsThreadsEdit, Decimals: 0, Row: 2, Column: 5},
													Label{Text: "默认超时", Row: 2, Column: 6, TextColor: guiColorMuted},
													NumberEdit{AssignTo: &app.settingsTimeoutEdit, Decimals: 0, Row: 2, Column: 7},

													Label{Text: "默认日志级别", Row: 3, Column: 0, TextColor: guiColorMuted},
													ComboBox{AssignTo: &app.settingsLogLevelCombo, Editable: false, Model: logLevelChoices, Row: 3, Column: 1},
													CheckBox{AssignTo: &app.settingsInfoCheck, Text: "默认仅获取信息", Row: 3, Column: 2},
													CheckBox{AssignTo: &app.settingsSeriesCheck, Text: "默认整季/全集", Row: 3, Column: 3},
													CheckBox{AssignTo: &app.settingsLowQualityCheck, Text: "默认最低质量", Row: 3, Column: 4},

													Label{Text: "任务并发", Row: 4, Column: 0, TextColor: guiColorMuted},
													NumberEdit{AssignTo: &app.settingsMaxJobsEdit, Decimals: 0, Row: 4, Column: 1},
													TextLabel{Text: fmt.Sprintf("同时运行的下载任务数，范围 1-%d。", maxGUIMaxJob), Row: 4, Column: 2, ColumnSpan: 6, MinSize: Size{220, 0}, TextColor: guiColorMuted},
												},
											},
										},
									},
									Composite{
										Background: SolidColorBrush{Color: guiColorCard},
										Border:     true,
										Layout:     VBox{Margins: Margins{14, 14, 14, 14}, Spacing: 10},
										Children: []Widget{
											Label{Text: "界面行为", Font: Font{Family: "Microsoft YaHei UI", PointSize: 11, Bold: true}, TextColor: guiColorText},
											TextLabel{Text: "这些选项控制预览、日志和任务选择的交互方式。", TextColor: guiColorMuted, MinSize: Size{260, 0}},
											Composite{Background: SolidColorBrush{Color: guiColorBorder}, MinSize: Size{0, 1}, MaxSize: Size{0, 1}},
											CheckBox{AssignTo: &app.settingsAutoPreviewCheck, Text: "粘贴链接后自动预览"},
											CheckBox{AssignTo: &app.settingsAutoOpenCheck, Text: "下载完成后自动打开输出目录"},
											CheckBox{AssignTo: &app.settingsAutoSelectCheck, Text: "新任务加入队列后自动选中"},
											CheckBox{AssignTo: &app.settingsFocusLogsCheck, Text: "任务失败时自动切换到日志页"},
										},
									},
									Composite{
										Background: SolidColorBrush{Color: guiColorCard},
										Border:     true,
										Layout:     HBox{Margins: Margins{14, 14, 14, 14}, Spacing: 10},
										Children: []Widget{
											TextLabel{AssignTo: &app.settingsStatusLabel, Text: "设置将保存到当前用户配置目录。", TextColor: guiColorMuted, MinSize: Size{280, 0}},
											HSpacer{},
											PushButton{Text: "恢复默认", OnClicked: app.resetSettingsPanel},
											PushButton{Text: "保存设置", OnClicked: app.saveSettingsPanel},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
func (app *desktopGUI) applySettingsToForm(cfg guiSettings) {
	app.outputDirEdit.SetText(cfg.OutputDir)
	app.workDirEdit.SetText(cfg.WorkDir)
	app.syncQualityChoices(cfg.Quality)
	app.retryEdit.SetValue(float64(cfg.Retry))
	app.threadsEdit.SetValue(float64(cfg.Threads))
	app.timeoutEdit.SetValue(float64(cfg.TimeoutSec))
	app.syncLogLevelChoices(cfg.LogLevel)
	app.infoCheck.SetChecked(cfg.Info)
	app.seriesCheck.SetChecked(cfg.Series)
	app.lowQualityCheck.SetChecked(cfg.LowQuality)
	app.applySettingsPanel(cfg)
}

func (app *desktopGUI) syncQualityChoices(current string) {
	setComboChoices(app.qualityCombo, qualityChoices, current, "自动选择")
	setComboChoices(app.settingsQualityCombo, qualityChoices, current, "自动选择")
}

func (app *desktopGUI) syncLogLevelChoices(current string) {
	setComboChoices(app.logLevelCombo, logLevelChoices, current, "info")
	setComboChoices(app.settingsLogLevelCombo, logLevelChoices, current, "info")
}

func (app *desktopGUI) applySettingsPanel(cfg guiSettings) {
	if app.settingsOutputDirEdit != nil {
		app.settingsOutputDirEdit.SetText(cfg.OutputDir)
	}
	if app.settingsWorkDirEdit != nil {
		app.settingsWorkDirEdit.SetText(cfg.WorkDir)
	}
	if app.settingsRetryEdit != nil {
		app.settingsRetryEdit.SetValue(float64(cfg.Retry))
	}
	if app.settingsThreadsEdit != nil {
		app.settingsThreadsEdit.SetValue(float64(cfg.Threads))
	}
	if app.settingsTimeoutEdit != nil {
		app.settingsTimeoutEdit.SetValue(float64(cfg.TimeoutSec))
	}
	if app.settingsMaxJobsEdit != nil {
		app.settingsMaxJobsEdit.SetValue(float64(cfg.MaxConcurrent))
	}
	if app.settingsInfoCheck != nil {
		app.settingsInfoCheck.SetChecked(cfg.Info)
	}
	if app.settingsSeriesCheck != nil {
		app.settingsSeriesCheck.SetChecked(cfg.Series)
	}
	if app.settingsLowQualityCheck != nil {
		app.settingsLowQualityCheck.SetChecked(cfg.LowQuality)
	}
	if app.settingsAutoPreviewCheck != nil {
		app.settingsAutoPreviewCheck.SetChecked(cfg.AutoPreview)
	}
	if app.settingsAutoOpenCheck != nil {
		app.settingsAutoOpenCheck.SetChecked(cfg.AutoOpenOutput)
	}
	if app.settingsAutoSelectCheck != nil {
		app.settingsAutoSelectCheck.SetChecked(cfg.AutoSelectNewest)
	}
	if app.settingsFocusLogsCheck != nil {
		app.settingsFocusLogsCheck.SetChecked(cfg.FocusLogsOnError)
	}
	if app.settingsStatusLabel != nil {
		_ = app.settingsStatusLabel.SetText("设置保存位置: " + app.cfgPath)
	}
}

func setComboChoices(combo *walk.ComboBox, base []string, current, defaultValue string) {
	if combo == nil {
		return
	}

	options := append([]string(nil), base...)
	current = strings.TrimSpace(current)
	if current != "" && !containsString(options, current) {
		options = append(options, current)
	}
	_ = combo.SetModel(options)
	if current == "" {
		_ = combo.SetText(defaultValue)
		return
	}
	_ = combo.SetText(current)
}

func (app *desktopGUI) chooseOutputDir() {
	path, err := app.chooseFolder("选择输出目录", app.outputDirEdit.Text())
	if err != nil {
		app.showError("选择输出目录失败", err)
		return
	}
	if path != "" {
		app.outputDirEdit.SetText(path)
	}
}

func (app *desktopGUI) chooseWorkDir() {
	path, err := app.chooseFolder("选择工作目录", app.workDirEdit.Text())
	if err != nil {
		app.showError("选择工作目录失败", err)
		return
	}
	if path != "" {
		app.workDirEdit.SetText(path)
	}
}

func (app *desktopGUI) chooseSettingsOutputDir() {
	initial := ""
	if app.settingsOutputDirEdit != nil {
		initial = app.settingsOutputDirEdit.Text()
	}
	path, err := app.chooseFolder("选择默认输出目录", initial)
	if err != nil {
		app.showError("选择默认输出目录失败", err)
		return
	}
	if path != "" && app.settingsOutputDirEdit != nil {
		app.settingsOutputDirEdit.SetText(path)
	}
}

func (app *desktopGUI) chooseSettingsWorkDir() {
	initial := ""
	if app.settingsWorkDirEdit != nil {
		initial = app.settingsWorkDirEdit.Text()
	}
	path, err := app.chooseFolder("选择默认工作目录", initial)
	if err != nil {
		app.showError("选择默认工作目录失败", err)
		return
	}
	if path != "" && app.settingsWorkDirEdit != nil {
		app.settingsWorkDirEdit.SetText(path)
	}
}

func (app *desktopGUI) saveSettingsPanel() {
	cfg, err := app.settingsFromPanel()
	if err != nil {
		app.showError("保存设置失败", err)
		return
	}

	app.mu.Lock()
	app.settings = cfg
	app.mu.Unlock()
	app.applyMaxConcurrent(cfg.MaxConcurrent)

	if err := saveGUISettings(app.cfgPath, cfg); err != nil {
		app.showError("保存设置失败", err)
		return
	}

	app.applySettingsToForm(cfg)
	app.setStatus(fmt.Sprintf("默认设置已保存，并同步到当前下载表单。当前并发上限 %d。", cfg.MaxConcurrent))
	if app.settingsStatusLabel != nil {
		_ = app.settingsStatusLabel.SetText("设置已保存: " + time.Now().Format("2006-01-02 15:04:05"))
	}
	if cfg.AutoPreview {
		app.onPreviewURLChanged()
	}
}

func (app *desktopGUI) resetSettingsPanel() {
	cfg := normalizeGUISettings(guiSettings{}, app.workDir)
	app.applySettingsToForm(cfg)
	app.applyMaxConcurrent(cfg.MaxConcurrent)
	if app.settingsStatusLabel != nil {
		_ = app.settingsStatusLabel.SetText("已恢复默认值，点击“保存设置”后写入磁盘。")
	}
	app.setStatus("设置面板已恢复为默认值。")
}

func (app *desktopGUI) settingsFromPanel() (guiSettings, error) {
	cfg := guiSettings{
		Version:          guiSettingsVersion,
		MaxConcurrent:    int(app.settingsMaxJobsEdit.Value()),
		OutputDir:        strings.TrimSpace(app.settingsOutputDirEdit.Text()),
		WorkDir:          strings.TrimSpace(app.settingsWorkDirEdit.Text()),
		Quality:          normalizeQualitySelection(app.settingsQualityCombo.Text()),
		Retry:            uint8(app.settingsRetryEdit.Value()),
		Threads:          uint8(app.settingsThreadsEdit.Value()),
		TimeoutSec:       int(app.settingsTimeoutEdit.Value()),
		Info:             app.settingsInfoCheck.Checked(),
		Series:           app.settingsSeriesCheck.Checked(),
		LowQuality:       app.settingsLowQualityCheck.Checked(),
		LogLevel:         strings.TrimSpace(app.settingsLogLevelCombo.Text()),
		AutoPreview:      app.settingsAutoPreviewCheck.Checked(),
		AutoOpenOutput:   app.settingsAutoOpenCheck.Checked(),
		AutoSelectNewest: app.settingsAutoSelectCheck.Checked(),
		FocusLogsOnError: app.settingsFocusLogsCheck.Checked(),
	}

	cfg = normalizeGUISettings(cfg, app.workDir)

	if cfg.WorkDir != "" {
		stat, err := os.Stat(cfg.WorkDir)
		if err != nil || !stat.IsDir() {
			return cfg, errors.New("默认工作目录不存在或不是文件夹")
		}
	}

	return cfg, nil
}

func (app *desktopGUI) chooseFolder(title, initial string) (string, error) {
	dlg := walk.FileDialog{
		Title:          title,
		InitialDirPath: strings.TrimSpace(initial),
	}

	accepted, err := dlg.ShowBrowseFolder(app.mw)
	if err != nil {
		return "", err
	}
	if !accepted {
		return "", nil
	}
	return strings.TrimSpace(dlg.FilePath), nil
}

func (app *desktopGUI) currentSettingsSnapshot() guiSettings {
	app.mu.RLock()
	defer app.mu.RUnlock()
	return app.settings
}

func (app *desktopGUI) showSideTab(index int) {
	if app.sideTabs == nil || index < 0 {
		return
	}
	app.queueUI(func() {
		_ = app.sideTabs.SetCurrentIndex(index)
	})
}

func (app *desktopGUI) maybeFocusLogsOnError() {
	if !app.currentSettingsSnapshot().FocusLogsOnError {
		return
	}
	app.showSideTab(sideTabLogs)
}

func (app *desktopGUI) maybeOpenOutputDir(path string) {
	cfg := app.currentSettingsSnapshot()
	path = strings.TrimSpace(path)
	if !cfg.AutoOpenOutput || path == "" {
		return
	}
	go func(outputDir string) {
		if err := openFolder(outputDir); err != nil {
			log.Warnf("auto open output dir failed: %v", err)
		}
	}(path)
}

func (app *desktopGUI) applyMaxConcurrent(v int) {
	if v < 1 {
		v = defaultGUIMaxJob
	}
	if v > maxGUIMaxJob {
		v = maxGUIMaxJob
	}

	app.runMu.Lock()
	app.maxJobs = v
	app.runMu.Unlock()
}

func (app *desktopGUI) stopAutoPreviewTimer() {
	app.autoPreviewMu.Lock()
	defer app.autoPreviewMu.Unlock()
	if app.autoPreviewTimer != nil {
		app.autoPreviewTimer.Stop()
		app.autoPreviewTimer = nil
	}
}

func (app *desktopGUI) openCurrentOutputDir() {
	path := strings.TrimSpace(app.outputDirEdit.Text())
	if task := app.selectedTask(); task != nil {
		snap := task.snapshot(false)
		if strings.TrimSpace(snap.OutputDir) != "" {
			path = snap.OutputDir
		}
	}
	if path == "" {
		path = app.settings.OutputDir
	}
	if path == "" {
		app.showInfo("打开目录", "当前没有可打开的输出目录。")
		return
	}
	if err := openFolder(path); err != nil {
		app.showError("打开目录失败", err)
	}
}

func (app *desktopGUI) startTaskFromForm() {
	req, err := app.requestFromForm()
	if err != nil {
		app.showError("参数校验失败", err)
		return
	}

	task := app.enqueueTask(req)
	app.maybeWarmPreview(req.URL)
	app.showSideTab(sideTabDetails)
	app.setStatus(fmt.Sprintf("任务 #%d 已加入队列。", task.id))
}

func (app *desktopGUI) requestFromForm() (desktopDownloadRequest, error) {
	req := desktopDownloadRequest{
		URL:        strings.TrimSpace(app.urlEdit.Text()),
		OutputDir:  strings.TrimSpace(app.outputDirEdit.Text()),
		WorkDir:    strings.TrimSpace(app.workDirEdit.Text()),
		Quality:    normalizeQualitySelection(app.qualityCombo.Text()),
		Retry:      uint8(app.retryEdit.Value()),
		Threads:    uint8(app.threadsEdit.Value()),
		TimeoutSec: int(app.timeoutEdit.Value()),
		Info:       app.infoCheck.Checked(),
		Series:     app.seriesCheck.Checked(),
		LowQuality: app.lowQualityCheck.Checked(),
		LogLevel:   strings.TrimSpace(app.logLevelCombo.Text()),
	}

	if req.URL == "" {
		return req, errors.New("请先填写视频链接")
	}
	if req.OutputDir == "" {
		req.OutputDir = app.settings.OutputDir
	}
	if req.OutputDir == "" {
		req.OutputDir = app.workDir
	}
	if err := os.MkdirAll(req.OutputDir, 0o755); err != nil {
		return req, fmt.Errorf("创建输出目录失败: %w", err)
	}

	if req.WorkDir == "" {
		req.WorkDir = app.settings.WorkDir
	}
	if req.WorkDir == "" {
		req.WorkDir = app.workDir
	}

	stat, err := os.Stat(req.WorkDir)
	if err != nil || !stat.IsDir() {
		return req, errors.New("工作目录不存在或不是文件夹")
	}
	if req.Threads < 1 || req.Threads > maxThreads {
		return req, fmt.Errorf("线程数必须在 1-%d 之间", maxThreads)
	}
	if req.LogLevel == "" {
		req.LogLevel = "info"
	}

	return req, nil
}

func (app *desktopGUI) enqueueTask(req desktopDownloadRequest) *desktopTask {
	id := app.next.Add(1)
	ctx, cancel := context.WithCancel(context.Background())

	task := &desktopTask{
		id:             id,
		req:            req,
		createdAt:      time.Now(),
		status:         desktopTaskQueued,
		logs:           make([]string, 0, 64),
		progressStatus: "queued",
		fileProgress:   make(map[string]float64),
		ctx:            ctx,
		cancel:         cancel,
	}

	app.mu.Lock()
	app.tasks[id] = task
	app.settings = guiSettingsFromRequest(req, app.settings)
	cfg := app.settings
	cfgPath := app.cfgPath
	app.mu.Unlock()

	if err := saveGUISettings(cfgPath, cfg); err != nil {
		log.Warnf("save GUI settings failed: %v", err)
	}

	app.queueUI(func() {
		app.taskModel.prependTask(task)
		if cfg.AutoSelectNewest || app.selectedTask() == nil {
			_ = app.taskView.SetCurrentIndex(0)
			app.onTaskSelectionChanged()
			return
		}
		app.updateButtons()
	})

	go app.runTask(task)
	return task
}

func (app *desktopGUI) runTask(task *desktopTask) {
	if !app.acquireRunSlot(task.ctx) {
		task.finish(desktopTaskCanceled, -1, "task canceled before start")
		app.refreshTask(task)
		return
	}
	defer app.releaseRunSlot()

	task.markRunning()
	app.refreshTask(task)

	args := make([]string, 0, 16)
	if task.req.LogLevel != "" {
		args = append(args, "--log-level", task.req.LogLevel)
	}
	args = append(args, "dl")
	if task.req.OutputDir != "" {
		args = append(args, "-o", task.req.OutputDir)
	}
	if task.req.Quality != "" {
		args = append(args, "-q", task.req.Quality)
	}
	if task.req.Info {
		args = append(args, "-i")
	}
	if task.req.LowQuality {
		args = append(args, "--low-quality")
	}
	if task.req.Series {
		args = append(args, "-s")
	}
	args = append(args, "--retry", strconv.Itoa(int(task.req.Retry)))
	args = append(args, "--threads", strconv.Itoa(int(task.req.Threads)))
	args = append(args, task.req.URL)

	cmdCtx := task.ctx
	cancelTimeout := func() {}
	if task.req.TimeoutSec > 0 {
		cmdCtx, cancelTimeout = context.WithTimeout(task.ctx, time.Duration(task.req.TimeoutSec)*time.Second)
	}
	defer cancelTimeout()

	cmd := exec.CommandContext(cmdCtx, app.executable, args...)
	cmd.Dir = task.req.WorkDir
	cmd.Env = append(os.Environ(), progressJSONEnv+"=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		task.finish(desktopTaskError, -1, err.Error())
		app.refreshTask(task)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		task.finish(desktopTaskError, -1, err.Error())
		app.refreshTask(task)
		return
	}

	task.appendLog("Exec: " + app.executable + " " + strings.Join(args, " "))
	app.refreshTask(task)

	if err := cmd.Start(); err != nil {
		task.finish(desktopTaskError, -1, err.Error())
		app.refreshTask(task)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go app.streamTaskLogs(&wg, stdout, task, "")
	go app.streamTaskLogs(&wg, stderr, task, "[stderr] ")

	waitErr := cmd.Wait()
	wg.Wait()

	if errors.Is(task.ctx.Err(), context.Canceled) {
		task.finish(desktopTaskCanceled, -1, "任务已取消")
		app.refreshTask(task)
		return
	}
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		task.finish(desktopTaskError, -1, fmt.Sprintf("任务超时：%d 秒", task.req.TimeoutSec))
		app.refreshTask(task)
		app.maybeFocusLogsOnError()
		return
	}
	if waitErr != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		task.finish(desktopTaskError, exitCode, waitErr.Error())
		app.refreshTask(task)
		app.maybeFocusLogsOnError()
		return
	}

	task.finish(desktopTaskDone, 0, "")
	app.refreshTask(task)
	if !task.req.Info {
		app.maybeOpenOutputDir(task.req.OutputDir)
	}
}

func (app *desktopGUI) streamTaskLogs(wg *sync.WaitGroup, r io.ReadCloser, task *desktopTask, prefix string) {
	defer wg.Done()
	defer r.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if app.parseAndApplyProgressLine(task, line) {
			continue
		}
		task.appendLog(prefix + line)
		app.refreshTask(task)
	}
	if err := scanner.Err(); err != nil {
		task.appendLog(prefix + "log read error: " + err.Error())
		app.refreshTask(task)
	}
}

func (app *desktopGUI) parseAndApplyProgressLine(task *desktopTask, line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, progressEventPrefix) {
		return false
	}

	payload := strings.TrimSpace(strings.TrimPrefix(line, progressEventPrefix))
	if payload == "" {
		return false
	}

	evt := webUIProgressLine{}
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		return false
	}

	task.applyProgress(evt)
	app.refreshTask(task)
	return true
}

func (app *desktopGUI) refreshTask(task *desktopTask) {
	if task == nil || app.closed.Load() {
		return
	}

	app.scheduleTaskRefresh(task.id)
}

func (app *desktopGUI) onTaskSelectionChanged() {
	task := app.selectedTask()
	app.refreshDetails(task)
	app.updateButtons()
}

func (app *desktopGUI) scheduleTaskRefresh(taskID int64) {
	if taskID <= 0 || app.closed.Load() {
		return
	}

	app.refreshMu.Lock()
	app.dirtyTaskIDs[taskID] = struct{}{}
	if app.refreshScheduled {
		app.refreshMu.Unlock()
		return
	}
	app.refreshScheduled = true
	app.refreshMu.Unlock()

	go func() {
		time.Sleep(guiRefreshInterval)
		app.flushTaskRefreshes()
	}()
}

func (app *desktopGUI) flushTaskRefreshes() {
	if app.closed.Load() {
		return
	}

	app.refreshMu.Lock()
	if len(app.dirtyTaskIDs) == 0 {
		app.refreshScheduled = false
		app.refreshMu.Unlock()
		return
	}
	dirty := make([]int64, 0, len(app.dirtyTaskIDs))
	for id := range app.dirtyTaskIDs {
		dirty = append(dirty, id)
	}
	app.dirtyTaskIDs = make(map[int64]struct{})
	app.refreshScheduled = false
	app.refreshMu.Unlock()

	app.queueUI(func() {
		for _, id := range dirty {
			if idx := app.taskModel.indexOf(id); idx >= 0 {
				app.taskModel.PublishRowChanged(idx)
			}
		}
		if current := app.selectedTask(); current != nil {
			for _, id := range dirty {
				if current.id == id {
					app.refreshDetails(current)
					break
				}
			}
		}
		app.updateButtons()

		now := time.Now()
		if app.lastStatusAt.IsZero() || now.Sub(app.lastStatusAt) >= guiStatusMinRefresh {
			app.lastStatusAt = now
			app.setStatus(app.summaryStatusText())
		}
	})
}

func (app *desktopGUI) selectedTask() *desktopTask {
	if app.taskView == nil || app.taskModel == nil {
		return nil
	}
	return app.taskModel.taskAt(app.taskView.CurrentIndex())
}

func (app *desktopGUI) refreshDetails(task *desktopTask) {
	if task == nil {
		app.progressBar.SetValue(0)
		_ = app.progressLabel.SetText("未选择任务")
		_ = app.summaryEdit.SetText("")
		_ = app.logEdit.SetText("")
		app.logViewTaskID = 0
		app.logViewLogCount = 0
		app.logViewLogVer = 0
		return
	}

	snap := task.snapshot(true)
	app.progressBar.SetValue(clampPercent(snap.Percent))
	_ = app.progressLabel.SetText(progressSummaryText(snap))
	_ = app.summaryEdit.SetText(formatTaskSummary(snap))
	app.refreshTaskLog(snap)
}

func (app *desktopGUI) updateButtons() {
	task := app.selectedTask()
	if task == nil {
		app.cancelBtn.SetEnabled(false)
		app.retryBtn.SetEnabled(false)
		return
	}

	snap := task.snapshot(false)
	active := snap.Status == string(desktopTaskQueued) || snap.Status == string(desktopTaskRunning)
	finished := snap.Status == string(desktopTaskDone) || snap.Status == string(desktopTaskError) || snap.Status == string(desktopTaskCanceled)

	app.cancelBtn.SetEnabled(active)
	app.retryBtn.SetEnabled(finished)
}

func (app *desktopGUI) cancelSelectedTask() {
	task := app.selectedTask()
	if task == nil {
		app.showInfo("取消任务", "请先选中一个任务。")
		return
	}
	if task.cancelIfActive() {
		app.setStatus(fmt.Sprintf("任务 #%d 已发送取消请求。", task.id))
		return
	}
	app.showInfo("取消任务", "当前任务不在运行中。")
}

func (app *desktopGUI) retrySelectedTask() {
	task := app.selectedTask()
	if task == nil {
		app.showInfo("重试任务", "请先选中一个任务。")
		return
	}

	snap := task.snapshot(false)
	if snap.Status == string(desktopTaskQueued) || snap.Status == string(desktopTaskRunning) {
		app.showInfo("重试任务", "运行中的任务不能重试。")
		return
	}

	req := desktopDownloadRequest{
		URL:        snap.URL,
		OutputDir:  snap.OutputDir,
		WorkDir:    snap.WorkDir,
		Quality:    snap.Quality,
		Retry:      snap.Retry,
		Threads:    snap.Threads,
		TimeoutSec: snap.TimeoutSec,
		Info:       snap.Info,
		LowQuality: snap.LowQuality,
		Series:     snap.Series,
		LogLevel:   snap.LogLevel,
	}
	newTask := app.enqueueTask(req)
	app.setStatus(fmt.Sprintf("已基于任务 #%d 创建重试任务 #%d。", task.id, newTask.id))
}

func (app *desktopGUI) clearFinishedTasks() {
	app.mu.Lock()
	kept := make([]*desktopTask, 0, len(app.tasks))
	for id, task := range app.tasks {
		status := task.snapshot(false).Status
		if status == string(desktopTaskDone) || status == string(desktopTaskError) || status == string(desktopTaskCanceled) {
			delete(app.tasks, id)
			continue
		}
		kept = append(kept, task)
	}
	app.mu.Unlock()

	app.queueUI(func() {
		app.taskModel.setTasks(sortTasksDesc(kept))
		if app.taskModel.RowCount() > 0 {
			_ = app.taskView.SetCurrentIndex(0)
			app.onTaskSelectionChanged()
			return
		}
		app.refreshDetails(nil)
		app.updateButtons()
		app.setStatus("已清理完成的任务。")
	})
}

func (app *desktopGUI) cancelAllTasks() {
	app.mu.RLock()
	tasks := make([]*desktopTask, 0, len(app.tasks))
	for _, task := range app.tasks {
		tasks = append(tasks, task)
	}
	app.mu.RUnlock()

	for _, task := range tasks {
		task.cancelIfActive()
	}
}

func (app *desktopGUI) acquireRunSlot(ctx context.Context) bool {
	ticker := time.NewTicker(180 * time.Millisecond)
	defer ticker.Stop()

	for {
		app.runMu.Lock()
		if app.runningJobs < app.maxJobs {
			app.runningJobs++
			app.runMu.Unlock()
			return true
		}
		app.runMu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (app *desktopGUI) releaseRunSlot() {
	app.runMu.Lock()
	if app.runningJobs > 0 {
		app.runningJobs--
	}
	app.runMu.Unlock()
}

func (app *desktopGUI) queueUI(fn func()) {
	if app.closed.Load() || app.mw == nil {
		return
	}
	app.mw.Synchronize(func() {
		if app.closed.Load() {
			return
		}
		fn()
	})
}

func (app *desktopGUI) setStatus(text string) {
	if app.statusLabel == nil {
		return
	}
	_ = app.statusLabel.SetText(text)
}

func (app *desktopGUI) refreshTaskLog(snap desktopTaskSnapshot) {
	if app.logEdit == nil {
		return
	}

	if app.logViewTaskID != snap.ID {
		_ = app.logEdit.SetText(strings.Join(snap.Logs, "\r\n"))
		app.logViewTaskID = snap.ID
		app.logViewLogCount = len(snap.Logs)
		app.logViewLogVer = snap.LogVer
		app.scrollLogToEnd()
		return
	}

	switch {
	case snap.LogVer == app.logViewLogVer:
		return
	case len(snap.Logs) == app.logViewLogCount:
		_ = app.logEdit.SetText(strings.Join(snap.Logs, "\r\n"))
	case len(snap.Logs) < app.logViewLogCount:
		_ = app.logEdit.SetText(strings.Join(snap.Logs, "\r\n"))
	default:
		extra := strings.Join(snap.Logs[app.logViewLogCount:], "\r\n")
		if extra != "" {
			if app.logViewLogCount > 0 {
				extra = "\r\n" + extra
			}
			appendTextEdit(app.logEdit, extra)
		}
	}

	app.logViewLogCount = len(snap.Logs)
	app.logViewLogVer = snap.LogVer
	app.scrollLogToEnd()
}

func (app *desktopGUI) scrollLogToEnd() {
	if app.logEdit == nil {
		return
	}
	app.logEdit.SetTextSelection(app.logEdit.TextLength(), app.logEdit.TextLength())
	app.logEdit.ScrollToCaret()
}

func appendTextEdit(te *walk.TextEdit, text string) {
	if te == nil || text == "" {
		return
	}

	readOnly := te.ReadOnly()
	if readOnly {
		_ = te.SetReadOnly(false)
	}
	te.AppendText(text)
	if readOnly {
		_ = te.SetReadOnly(true)
	}
}

func (app *desktopGUI) summaryStatusText() string {
	app.mu.RLock()
	defer app.mu.RUnlock()

	total := len(app.tasks)
	running := 0
	queued := 0
	for _, task := range app.tasks {
		status := task.snapshot(false).Status
		switch status {
		case string(desktopTaskRunning):
			running++
		case string(desktopTaskQueued):
			queued++
		}
	}

	app.runMu.Lock()
	maxJobs := app.maxJobs
	app.runMu.Unlock()

	return fmt.Sprintf("任务总数 %d，运行中 %d，排队中 %d，并发上限 %d。", total, running, queued, maxJobs)
}

func (app *desktopGUI) showError(title string, err error) {
	if err == nil {
		return
	}
	walk.MsgBox(app.mw, title, err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
}

func (app *desktopGUI) showInfo(title, text string) {
	walk.MsgBox(app.mw, title, text, walk.MsgBoxOK|walk.MsgBoxIconInformation)
}

func (t *desktopTask) applyProgress(evt webUIProgressLine) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.fileProgress == nil {
		t.fileProgress = make(map[string]float64)
	}

	ratio := evt.Ratio
	if ratio <= 0 && evt.Percent > 0 {
		ratio = float64(evt.Percent) / 100
	}
	switch {
	case ratio < 0:
		ratio = 0
	case ratio > 1:
		ratio = 1
	}

	fileKey := strings.TrimSpace(evt.File)
	if fileKey == "" {
		fileKey = "_task"
	}
	if ratio > 0 {
		t.fileProgress[fileKey] = ratio
	}

	sum := 0.0
	count := 0.0
	for _, r := range t.fileProgress {
		sum += r
		count++
	}
	if count > 0 {
		t.progressRatio = sum / count
	}
	if t.progressRatio < 0 {
		t.progressRatio = 0
	}
	if t.progressRatio > 1 {
		t.progressRatio = 1
	}

	if evt.Status != "" {
		t.progressStatus = evt.Status
	}
	if evt.Speed > 0 {
		t.progressSpeed = evt.Speed
	}
	if evt.RemainingS >= 0 {
		t.progressRemainingS = evt.RemainingS
	}
	if strings.TrimSpace(evt.File) != "" {
		t.progressFile = evt.File
	}
}

func (t *desktopTask) appendLog(line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.logs) >= maxTaskLogLines {
		t.logs = append(t.logs[:0], t.logs[1:]...)
	}
	t.logs = append(t.logs, trimmed)
	t.logVer++
}

func (t *desktopTask) markRunning() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = desktopTaskRunning
	t.startedAt = time.Now()
	if t.progressStatus == "" || t.progressStatus == "queued" {
		t.progressStatus = "downloading"
	}
}

func (t *desktopTask) finish(status desktopTaskStatus, exitCode int, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = status
	t.exitCode = exitCode
	t.errMsg = strings.TrimSpace(errMsg)
	t.finishedAt = time.Now()
	t.cancel = nil

	switch status {
	case desktopTaskDone:
		t.progressRatio = 1
		t.progressStatus = "complete"
		t.progressRemainingS = 0
	case desktopTaskError:
		t.progressStatus = "error"
	case desktopTaskCanceled:
		t.progressStatus = "canceled"
	}
}

func (t *desktopTask) cancelIfActive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.status != desktopTaskQueued && t.status != desktopTaskRunning {
		return false
	}
	if t.cancel == nil {
		return false
	}

	t.cancel()
	return true
}

func (t *desktopTask) snapshot(includeLogs bool) desktopTaskSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap := desktopTaskSnapshot{
		ID:         t.id,
		URL:        t.req.URL,
		OutputDir:  t.req.OutputDir,
		WorkDir:    t.req.WorkDir,
		Quality:    t.req.Quality,
		Retry:      t.req.Retry,
		Threads:    t.req.Threads,
		TimeoutSec: t.req.TimeoutSec,
		Info:       t.req.Info,
		LowQuality: t.req.LowQuality,
		Series:     t.req.Series,
		LogLevel:   t.req.LogLevel,
		Status:     string(t.status),
		Error:      t.errMsg,
		ExitCode:   t.exitCode,
		Progress:   t.progressRatio,
		Percent:    clampPercent(int(t.progressRatio*100 + 0.5)),
		PStage:     t.progressStatus,
		PFile:      t.progressFile,
		PSpeed:     t.progressSpeed,
		PRemainS:   t.progressRemainingS,
		CreatedAt:  t.createdAt,
		StartedAt:  t.startedAt,
		FinishedAt: t.finishedAt,
	}
	if includeLogs {
		snap.Logs = append([]string(nil), t.logs...)
	}
	snap.LogVer = t.logVer
	return snap
}

func guiSettingsFromRequest(req desktopDownloadRequest, base guiSettings) guiSettings {
	cfg := base
	cfg.Version = guiSettingsVersion
	cfg.MaxConcurrent = base.MaxConcurrent
	cfg.OutputDir = req.OutputDir
	cfg.WorkDir = req.WorkDir
	cfg.Quality = req.Quality
	cfg.Retry = req.Retry
	cfg.Threads = req.Threads
	cfg.TimeoutSec = req.TimeoutSec
	cfg.Info = req.Info
	cfg.Series = req.Series
	cfg.LowQuality = req.LowQuality
	cfg.LogLevel = req.LogLevel
	return cfg
}

func normalizeGUISettings(in guiSettings, fallbackWorkDir string) guiSettings {
	cfg := in
	if cfg.Version <= 0 {
		cfg.AutoSelectNewest = true
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = defaultGUIMaxJob
	}
	if cfg.MaxConcurrent > maxGUIMaxJob {
		cfg.MaxConcurrent = maxGUIMaxJob
	}
	if strings.TrimSpace(cfg.OutputDir) == "" {
		cfg.OutputDir = fallbackWorkDir
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		cfg.WorkDir = fallbackWorkDir
	}
	cfg.Quality = strings.TrimSpace(cfg.Quality)
	if cfg.Retry == 0 {
		cfg.Retry = defaultRetries
	}
	if cfg.Threads == 0 {
		cfg.Threads = defaultThreads
	}
	if cfg.Threads > maxThreads {
		cfg.Threads = maxThreads
	}
	if cfg.TimeoutSec < 0 {
		cfg.TimeoutSec = 0
	}
	if strings.TrimSpace(cfg.LogLevel) == "" {
		cfg.LogLevel = "info"
	}
	cfg.Version = guiSettingsVersion
	return cfg
}

func defaultGUISettingsPath() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "hanime-hunter", "gui-settings.json"), nil
}

func loadGUISettings(path string) (guiSettings, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		cfg := guiSettings{}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return guiSettings{}, err
		}
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return guiSettings{}, err
	}

	legacyPath, legacyErr := defaultWebUISettingsPath()
	if legacyErr != nil {
		return guiSettings{}, nil
	}

	legacy, legacyErr := loadWebUISettings(legacyPath)
	if legacyErr != nil {
		return guiSettings{}, nil
	}

	return guiSettings{
		Version:          guiSettingsVersion,
		MaxConcurrent:    defaultGUIMaxJob,
		OutputDir:        legacy.OutputDir,
		WorkDir:          legacy.WorkDir,
		Quality:          legacy.Quality,
		Retry:            legacy.Retry,
		Threads:          legacy.Threads,
		TimeoutSec:       legacy.TimeoutSec,
		Info:             legacy.Info,
		Series:           legacy.Series,
		LowQuality:       legacy.LowQuality,
		LogLevel:         legacy.LogLevel,
		AutoSelectNewest: true,
	}, nil
}

func saveGUISettings(path string, cfg guiSettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func desktopStatusLabel(status string) string {
	switch status {
	case string(desktopTaskQueued):
		return "排队中"
	case string(desktopTaskRunning):
		return "运行中"
	case string(desktopTaskDone):
		return "已完成"
	case string(desktopTaskError):
		return "失败"
	case string(desktopTaskCanceled):
		return "已取消"
	default:
		return status
	}
}

func desktopStageLabel(stage string) string {
	switch stage {
	case "queued":
		return "排队中"
	case "downloading", "下载中":
		return "下载中"
	case "merging", "合并中":
		return "合并中"
	case "retry", "retrying", "重试中":
		return "重试中"
	case "complete", "已完成":
		return "已完成"
	case "error", "失败":
		return "失败"
	case "canceled", "已取消":
		return "已取消"
	default:
		if strings.TrimSpace(stage) == "" {
			return "-"
		}
		return stage
	}
}

func progressSummaryText(snap desktopTaskSnapshot) string {
	parts := []string{
		fmt.Sprintf("%d%%", snap.Percent),
		desktopStageLabel(snap.PStage),
	}
	if snap.PSpeed > 0 {
		parts = append(parts, formatSpeed(snap.PSpeed))
	}
	if snap.PRemainS > 0 {
		parts = append(parts, fmt.Sprintf("剩余 %ds", snap.PRemainS))
	}
	if snap.PFile != "" {
		parts = append(parts, trimDisplay(snap.PFile, 48))
	}
	return strings.Join(parts, " | ")
}

func formatTaskSummary(snap desktopTaskSnapshot) string {
	lines := []string{
		"任务信息",
		"编号: " + strconv.FormatInt(snap.ID, 10),
		"状态: " + desktopStatusLabel(snap.Status),
		"阶段: " + desktopStageLabel(snap.PStage),
		"进度: " + fmt.Sprintf("%d%%", snap.Percent),
		"链接: " + snap.URL,
		"输出目录: " + snap.OutputDir,
		"工作目录: " + snap.WorkDir,
		"质量: " + displayQuality(snap.Quality),
		"线程: " + strconv.Itoa(int(snap.Threads)),
		"重试: " + strconv.Itoa(int(snap.Retry)),
		"日志级别: " + snap.LogLevel,
		"仅信息: " + boolText(snap.Info),
		"整季/全集: " + boolText(snap.Series),
		"最低质量: " + boolText(snap.LowQuality),
	}

	if snap.PFile != "" {
		lines = append(lines, "当前文件: "+snap.PFile)
	}
	if snap.PSpeed > 0 {
		lines = append(lines, "下载速度: "+formatSpeed(snap.PSpeed))
	}
	if snap.PRemainS > 0 {
		lines = append(lines, fmt.Sprintf("预计剩余: %d 秒", snap.PRemainS))
	}
	if snap.ExitCode != 0 {
		lines = append(lines, "退出码: "+strconv.Itoa(snap.ExitCode))
	}
	if snap.Error != "" {
		lines = append(lines, "错误: "+snap.Error)
	}
	if !snap.StartedAt.IsZero() {
		lines = append(lines, "开始时间: "+snap.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if !snap.FinishedAt.IsZero() {
		lines = append(lines, "结束时间: "+snap.FinishedAt.Format("2006-01-02 15:04:05"))
	}

	return strings.Join(lines, "\r\n")
}

func taskPrimaryText(snap desktopTaskSnapshot) string {
	if strings.TrimSpace(snap.PFile) != "" {
		return snap.PFile
	}
	if strings.TrimSpace(snap.URL) != "" {
		return snap.URL
	}
	return "-"
}

func sortTasksDesc(tasks []*desktopTask) []*desktopTask {
	out := append([]*desktopTask(nil), tasks...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].id > out[i].id {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func openFolder(path string) error {
	cmd := exec.Command("explorer", path)
	return cmd.Start()
}

func clampPercent(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

func displayQuality(q string) string {
	if strings.TrimSpace(q) == "" {
		return "自动选择"
	}
	return q
}

func normalizeQualitySelection(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "自动选择" {
		return ""
	}
	return value
}

func formatSpeed(speed int64) string {
	if speed <= 0 {
		return "-"
	}

	const unit = 1024
	if speed < unit {
		return fmt.Sprintf("%d B/s", speed)
	}

	div, exp := int64(unit), 0
	for n := speed / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB/s", float64(speed)/float64(div), "KMGTPE"[exp])
}

func trimDisplay(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func boolText(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func init() {
	guiCmd.Flags().StringVar(&guiWorkDir, "workdir", "", "GUI 默认工作目录")
	guiCmd.Flags().IntVar(&guiMaxJobs, "max-concurrent", 0, "最多同时运行的任务数")
}
