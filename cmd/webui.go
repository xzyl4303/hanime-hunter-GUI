package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
)

const (
	maxTaskLogLines = 2000
)

var (
	webUIHost    string
	webUIPort    int
	webUIOpen    bool
	webUIWorkDir string
	webUIMaxJobs int
)

var webUICmd = &cobra.Command{
	Use:   "webui",
	Short: "Start web UI for downloads",
	Long:  "Start a browser-based UI to create and manage download tasks.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebUI()
	},
}

type taskStatus string

const (
	taskQueued   taskStatus = "queued"
	taskRunning  taskStatus = "running"
	taskDone     taskStatus = "done"
	taskError    taskStatus = "error"
	taskCanceled taskStatus = "canceled"
)

type webUIDownloadRequest struct {
	URL        string `json:"url"`
	OutputDir  string `json:"outputDir"`
	WorkDir    string `json:"workDir"`
	Quality    string `json:"quality"`
	Retry      uint8  `json:"retry"`
	Threads    uint8  `json:"threads"`
	TimeoutSec int    `json:"timeoutSec"`
	Info       bool   `json:"info"`
	LowQuality bool   `json:"lowQuality"`
	Series     bool   `json:"series"`
	LogLevel   string `json:"logLevel"`
}

type downloadTask struct {
	id int64

	req        webUIDownloadRequest
	createdAt  time.Time
	startedAt  time.Time
	finishedAt time.Time

	status   taskStatus
	errMsg   string
	exitCode int
	logs     []string

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

type taskSnapshot struct {
	ID         int64     `json:"id"`
	URL        string    `json:"url"`
	OutputDir  string    `json:"outputDir"`
	WorkDir    string    `json:"workDir"`
	Quality    string    `json:"quality"`
	Retry      uint8     `json:"retry"`
	Threads    uint8     `json:"threads"`
	TimeoutSec int       `json:"timeoutSec"`
	Info       bool      `json:"info"`
	LowQuality bool      `json:"lowQuality"`
	Series     bool      `json:"series"`
	LogLevel   string    `json:"logLevel"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	ExitCode   int       `json:"exitCode,omitempty"`
	Progress   float64   `json:"progress"`
	Percent    int       `json:"progressPercent"`
	PStage     string    `json:"progressStage"`
	PFile      string    `json:"progressFile,omitempty"`
	PSpeed     int64     `json:"progressSpeed,omitempty"`
	PRemainS   int64     `json:"progressRemainingSec,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Logs       []string  `json:"logs,omitempty"`
}

type webUIProgressLine struct {
	File       string  `json:"file"`
	Ratio      float64 `json:"ratio"`
	Percent    int     `json:"percent"`
	Status     string  `json:"status"`
	Speed      int64   `json:"speed"`
	RemainingS int64   `json:"remainingSec"`
	Downloaded int64   `json:"downloaded"`
	Total      int64   `json:"total"`
}

type webUIServer struct {
	executable string
	workDir    string
	maxJobs    int
	sem        chan struct{}
	settings   webUISettings
	cfgPath    string

	mu    sync.RWMutex
	tasks map[int64]*downloadTask
	next  atomic.Int64
}

type indexData struct {
	DefaultOutputDir string
	DefaultWorkDir   string
	DefaultQuality   string
	DefaultRetry     uint8
	DefaultThreads   uint8
	DefaultTimeout   int
	DefaultInfo      bool
	DefaultSeries    bool
	DefaultLowQ      bool
	DefaultLogLevel  string
	MaxJobs          int
}

type webUISettings struct {
	OutputDir  string `json:"outputDir"`
	WorkDir    string `json:"workDir"`
	Quality    string `json:"quality"`
	Retry      uint8  `json:"retry"`
	Threads    uint8  `json:"threads"`
	TimeoutSec int    `json:"timeoutSec"`
	Info       bool   `json:"info"`
	Series     bool   `json:"series"`
	LowQuality bool   `json:"lowQuality"`
	LogLevel   string `json:"logLevel"`
}

func runWebUI() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	workDir := webUIWorkDir
	if strings.TrimSpace(workDir) == "" {
		workDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve working directory: %w", err)
		}
	}

	addr := net.JoinHostPort(webUIHost, strconv.Itoa(webUIPort))
	uiURL := "http://" + addr
	maxJobs := webUIMaxJobs
	if maxJobs < 1 {
		maxJobs = 1
	}
	cfgPath, pathErr := defaultWebUISettingsPath()
	if pathErr != nil {
		return fmt.Errorf("resolve settings path: %w", pathErr)
	}
	settings, loadErr := loadWebUISettings(cfgPath)
	if loadErr != nil {
		log.Warnf("Load settings failed: %v", loadErr)
	}
	settings = normalizeWebUISettings(settings, workDir)

	srv := &webUIServer{
		executable: executable,
		workDir:    workDir,
		maxJobs:    maxJobs,
		sem:        make(chan struct{}, maxJobs),
		settings:   settings,
		cfgPath:    cfgPath,
		tasks:      make(map[int64]*downloadTask),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/tasks", srv.handleTasks)
	mux.HandleFunc("/api/tasks/", srv.handleTaskByID)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Infof("Web UI started: %s", uiURL)
	log.Infof("Working directory: %s", workDir)
	log.Infof("Max concurrent tasks: %d", maxJobs)
	log.Infof("Settings file: %s", cfgPath)
	if webUIOpen {
		go func() {
			if openErr := openBrowser(uiURL); openErr != nil {
				log.Warnf("Open browser failed: %v", openErr)
			}
		}()
	}

	err = httpSrv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err //nolint:wrapcheck
}

func (s *webUIServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tmpl, err := template.New("index").Parse(webUIPageTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.mu.RLock()
	cfg := s.settings
	s.mu.RUnlock()
	_ = tmpl.Execute(w, indexData{
		DefaultOutputDir: cfg.OutputDir,
		DefaultWorkDir:   cfg.WorkDir,
		DefaultQuality:   cfg.Quality,
		DefaultRetry:     cfg.Retry,
		DefaultThreads:   cfg.Threads,
		DefaultTimeout:   cfg.TimeoutSec,
		DefaultInfo:      cfg.Info,
		DefaultSeries:    cfg.Series,
		DefaultLowQ:      cfg.LowQuality,
		DefaultLogLevel:  cfg.LogLevel,
		MaxJobs:          s.maxJobs,
	})
}

func (s *webUIServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleTaskList(w)
	case http.MethodPost:
		s.handleTaskCreate(w, r)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *webUIServer) handleTaskList(w http.ResponseWriter) {
	s.mu.RLock()
	snaps := make([]taskSnapshot, 0, len(s.tasks))
	for _, t := range s.tasks {
		snaps = append(snaps, t.snapshot(false))
	}
	s.mu.RUnlock()

	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].ID > snaps[j].ID
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": snaps,
	})
}

func (s *webUIServer) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	req := webUIDownloadRequest{}
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	req.URL = strings.TrimSpace(req.URL)
	req.OutputDir = strings.TrimSpace(req.OutputDir)
	req.WorkDir = strings.TrimSpace(req.WorkDir)
	req.Quality = strings.TrimSpace(req.Quality)
	req.LogLevel = strings.TrimSpace(req.LogLevel)
	s.mu.RLock()
	cfg := s.settings
	s.mu.RUnlock()

	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	if req.OutputDir == "" {
		req.OutputDir = cfg.OutputDir
	}
	if req.WorkDir == "" {
		req.WorkDir = cfg.WorkDir
	}
	if req.Quality == "" {
		req.Quality = cfg.Quality
	}
	if req.Retry == 0 {
		req.Retry = cfg.Retry
	}
	if req.Threads == 0 {
		req.Threads = cfg.Threads
	}
	if req.Threads < 1 || req.Threads > maxThreads {
		http.Error(w, "threads must be between 1 and 64", http.StatusBadRequest)
		return
	}
	if req.TimeoutSec < 0 {
		http.Error(w, "timeoutSec must be >= 0", http.StatusBadRequest)
		return
	}
	if req.LogLevel == "" {
		req.LogLevel = cfg.LogLevel
	}
	if stat, err := os.Stat(req.WorkDir); err != nil || !stat.IsDir() {
		http.Error(w, "workDir does not exist or is not a directory", http.StatusBadRequest)
		return
	}

	task := s.enqueueTask(req)
	writeJSON(w, http.StatusAccepted, task.snapshot(false))
}

func (s *webUIServer) enqueueTask(req webUIDownloadRequest) *downloadTask {
	id := s.next.Add(1)
	ctx, cancel := context.WithCancel(context.Background())

	task := &downloadTask{
		id:             id,
		req:            req,
		createdAt:      time.Now(),
		status:         taskQueued,
		logs:           make([]string, 0, 64),
		progressStatus: "queued",
		fileProgress:   make(map[string]float64),
		ctx:            ctx,
		cancel:         cancel,
	}

	s.mu.Lock()
	s.tasks[id] = task
	s.mu.Unlock()
	s.updateSettingsFromRequest(req)

	go s.runTask(task)
	return task
}

func (s *webUIServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	if strings.Trim(path, "/") == "finished" {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", http.MethodDelete)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		deleted := s.clearFinishedTasks()
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted": deleted,
		})
		return
	}

	if strings.HasSuffix(path, "/cancel") {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		idPart := strings.TrimSuffix(path, "/cancel")
		idPart = strings.TrimSuffix(idPart, "/")
		id, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		s.handleTaskCancel(w, id)
		return
	}

	if strings.HasSuffix(path, "/retry") {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		idPart := strings.TrimSuffix(path, "/retry")
		idPart = strings.TrimSuffix(idPart, "/")
		id, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		s.handleTaskRetry(w, id)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.ParseInt(strings.Trim(path, "/"), 10, 64)
	if err != nil {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	task, ok := s.tasks[id]
	s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	writeJSON(w, http.StatusOK, task.snapshot(true))
}

func (s *webUIServer) handleTaskCancel(w http.ResponseWriter, id int64) {
	s.mu.RLock()
	task, ok := s.tasks[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	if task.cancelIfActive() {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"id":      id,
			"status":  "canceling",
			"message": "cancel requested",
		})
		return
	}

	writeJSON(w, http.StatusConflict, map[string]any{
		"id":      id,
		"status":  task.snapshot(false).Status,
		"message": "task is not active",
	})
}

func (s *webUIServer) handleTaskRetry(w http.ResponseWriter, id int64) {
	s.mu.RLock()
	task, ok := s.tasks[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	orig := task.snapshot(false)
	if orig.Status == string(taskQueued) || orig.Status == string(taskRunning) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"id":      id,
			"status":  orig.Status,
			"message": "task is active",
		})
		return
	}

	if stat, err := os.Stat(orig.WorkDir); err != nil || !stat.IsDir() {
		http.Error(w, "workDir does not exist or is not a directory", http.StatusBadRequest)
		return
	}

	req := webUIDownloadRequest{
		URL:        orig.URL,
		OutputDir:  orig.OutputDir,
		WorkDir:    orig.WorkDir,
		Quality:    orig.Quality,
		Retry:      orig.Retry,
		Threads:    orig.Threads,
		TimeoutSec: orig.TimeoutSec,
		Info:       orig.Info,
		LowQuality: orig.LowQuality,
		Series:     orig.Series,
		LogLevel:   orig.LogLevel,
	}
	newTask := s.enqueueTask(req)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"retriedFrom": id,
		"task":        newTask.snapshot(false),
	})
}

func (s *webUIServer) clearFinishedTasks() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	deleted := 0
	for id, task := range s.tasks {
		status := task.snapshot(false).Status
		if status == string(taskDone) || status == string(taskError) || status == string(taskCanceled) {
			delete(s.tasks, id)
			deleted++
		}
	}

	return deleted
}

func (s *webUIServer) runTask(task *downloadTask) {
	if !s.acquireRunSlot(task.ctx) {
		task.finish(taskCanceled, -1, "task canceled before start")
		return
	}
	defer s.releaseRunSlot()

	task.markRunning()

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

	cmd := exec.CommandContext(cmdCtx, s.executable, args...)
	cmd.Dir = s.workDir
	cmd.Env = append(os.Environ(), progressJSONEnv+"=1")
	if task.req.WorkDir != "" {
		cmd.Dir = task.req.WorkDir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		task.finish(taskError, -1, err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		task.finish(taskError, -1, err.Error())
		return
	}

	task.appendLog("Exec: " + s.executable + " " + strings.Join(args, " "))

	if err := cmd.Start(); err != nil {
		task.finish(taskError, -1, err.Error())
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamTaskLogs(&wg, stdout, task, "")
	go streamTaskLogs(&wg, stderr, task, "[stderr] ")

	waitErr := cmd.Wait()
	wg.Wait()

	if errors.Is(task.ctx.Err(), context.Canceled) {
		task.finish(taskCanceled, -1, "task canceled")
		return
	}
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		task.finish(taskError, -1, fmt.Sprintf("task timeout after %d seconds", task.req.TimeoutSec))
		return
	}

	if waitErr != nil {
		exitCode := -1
		if exitErr := new(exec.ExitError); errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		task.finish(taskError, exitCode, waitErr.Error())
		return
	}

	task.finish(taskDone, 0, "")
}

func (s *webUIServer) acquireRunSlot(ctx context.Context) bool {
	select {
	case s.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *webUIServer) releaseRunSlot() {
	select {
	case <-s.sem:
	default:
	}
}

func streamTaskLogs(wg *sync.WaitGroup, r io.ReadCloser, task *downloadTask, prefix string) {
	defer wg.Done()
	defer r.Close()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if parseAndApplyProgressLine(task, line) {
			continue
		}
		task.appendLog(prefix + line)
	}
	if err := scanner.Err(); err != nil {
		task.appendLog(prefix + "log read error: " + err.Error())
	}
}

func parseAndApplyProgressLine(task *downloadTask, line string) bool {
	if task == nil {
		return false
	}

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
	return true
}

func (t *downloadTask) applyProgress(evt webUIProgressLine) {
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

func (t *downloadTask) appendLog(line string) {
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
}

func (t *downloadTask) markRunning() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = taskRunning
	t.startedAt = time.Now()
	if t.progressStatus == "" || t.progressStatus == "queued" {
		t.progressStatus = "downloading"
	}
}

func (t *downloadTask) finish(status taskStatus, exitCode int, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = status
	t.exitCode = exitCode
	t.errMsg = strings.TrimSpace(errMsg)
	t.finishedAt = time.Now()
	t.cancel = nil

	switch status {
	case taskDone:
		t.progressRatio = 1
		t.progressStatus = "complete"
		t.progressRemainingS = 0
	case taskError:
		t.progressStatus = "error"
	case taskCanceled:
		t.progressStatus = "canceled"
	}
}

func (t *downloadTask) cancelIfActive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.status != taskQueued && t.status != taskRunning {
		return false
	}
	if t.cancel == nil {
		return false
	}

	t.cancel()
	return true
}

func (t *downloadTask) snapshot(includeLogs bool) taskSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	res := taskSnapshot{
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
		Percent:    int(t.progressRatio*100 + 0.5),
		PStage:     t.progressStatus,
		PFile:      t.progressFile,
		PSpeed:     t.progressSpeed,
		PRemainS:   t.progressRemainingS,
		CreatedAt:  t.createdAt,
	}

	if !t.startedAt.IsZero() {
		res.StartedAt = t.startedAt
	}
	if !t.finishedAt.IsZero() {
		res.FinishedAt = t.finishedAt
	}
	if includeLogs {
		res.Logs = append([]string(nil), t.logs...)
	}

	return res
}

func (s *webUIServer) updateSettingsFromRequest(req webUIDownloadRequest) {
	s.mu.Lock()
	s.settings = webUISettings{
		OutputDir:  req.OutputDir,
		WorkDir:    req.WorkDir,
		Quality:    req.Quality,
		Retry:      req.Retry,
		Threads:    req.Threads,
		TimeoutSec: req.TimeoutSec,
		Info:       req.Info,
		Series:     req.Series,
		LowQuality: req.LowQuality,
		LogLevel:   req.LogLevel,
	}
	cfg := s.settings
	path := s.cfgPath
	s.mu.Unlock()

	if err := saveWebUISettings(path, cfg); err != nil {
		log.Warnf("Save settings failed: %v", err)
	}
}

func normalizeWebUISettings(in webUISettings, fallbackWorkDir string) webUISettings {
	cfg := in
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
	return cfg
}

func defaultWebUISettingsPath() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "hanime-hunter", "webui-settings.json"), nil
}

func loadWebUISettings(path string) (webUISettings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return webUISettings{}, nil
		}
		return webUISettings{}, err
	}

	cfg := webUISettings{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return webUISettings{}, err
	}
	return cfg, nil
}

func saveWebUISettings(path string, cfg webUISettings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start() //nolint:wrapcheck
}

func init() {
	webUICmd.Flags().StringVar(&webUIHost, "host", "127.0.0.1", "web ui listen host")
	webUICmd.Flags().IntVar(&webUIPort, "port", 8787, "web ui listen port")
	webUICmd.Flags().BoolVar(&webUIOpen, "open", true, "open browser automatically")
	webUICmd.Flags().StringVar(&webUIWorkDir, "workdir", "", "task working directory")
	webUICmd.Flags().IntVar(&webUIMaxJobs, "max-concurrent", 2, "maximum number of running tasks")
}

const webUIPageTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Hani Web UI</title>
  <style>
    :root {
      --bg: #f4f8ff;
      --panel: #ffffff;
      --panel-strong: #f6faff;
      --text: #0c1830;
      --muted: #566886;
      --accent: #1e63e9;
      --ok: #1e9f57;
      --warn: #d99000;
      --danger: #d33d4b;
      --border: #d6e4fb;
      --shadow: 0 14px 34px rgba(20, 70, 145, 0.12);
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #070707;
        --panel: #111111;
        --panel-strong: #171717;
        --text: #f3f3f3;
        --muted: #b6b6b6;
        --accent: #ffffff;
        --ok: #d8d8d8;
        --warn: #e7e7e7;
        --danger: #f2f2f2;
        --border: #2c2c2c;
        --shadow: 0 18px 36px rgba(0, 0, 0, 0.5);
      }
    }
    * { box-sizing: border-box; }
    html, body {
      margin: 0;
      background:
        radial-gradient(1100px 460px at 88% -16%, rgba(80, 135, 228, 0.18), transparent 62%),
        radial-gradient(900px 380px at 0% 0%, rgba(60, 120, 220, 0.10), transparent 56%),
        var(--bg);
      color: var(--text);
      font-family: "Segoe UI Variable", "Segoe UI", "Noto Sans", sans-serif;
    }
    .wrap {
      max-width: 1320px;
      margin: 0 auto;
      padding: 18px 24px 24px;
      display: grid;
      gap: 16px;
      grid-template-columns: 360px 1fr;
      align-items: start;
    }
    .card {
      background: linear-gradient(180deg, var(--panel-strong), var(--panel));
      border: 1px solid var(--border);
      border-radius: 16px;
      box-shadow: var(--shadow);
      overflow: hidden;
    }
    .card h2 {
      margin: 0;
      padding: 14px 16px;
      font-size: 14px;
      letter-spacing: .3px;
      background: rgba(255,255,255,.35);
      border-bottom: 1px solid var(--border);
    }
    .pad { padding: 16px; }
    label {
      display: block;
      margin-top: 11px;
      margin-bottom: 6px;
      color: var(--muted);
      font-size: 12px;
      letter-spacing: .2px;
    }
    input[type=text], input[type=number], select {
      width: 100%;
      background: var(--panel);
      color: var(--text);
      border: 1px solid var(--border);
      border-radius: 10px;
      font-size: 14px;
      padding: 10px 11px;
      outline: none;
      transition: border-color .14s ease, box-shadow .14s ease, transform .14s ease;
    }
    input:focus, select:focus {
      border-color: var(--accent);
      box-shadow: 0 0 0 3px rgba(30, 99, 233, .18);
      transform: translateY(-1px);
    }
    @media (prefers-color-scheme: dark) {
      input:focus, select:focus {
        box-shadow: 0 0 0 3px rgba(255,255,255,.24);
      }
    }
    .checks {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 8px 12px;
      margin-top: 8px;
    }
    .check {
      display: flex;
      align-items: center;
      gap: 8px;
      color: var(--muted);
      font-size: 13px;
    }
    .check input {
      accent-color: var(--accent);
      width: 16px;
      height: 16px;
    }
    .actions {
      margin-top: 14px;
      display: flex;
      flex-wrap: wrap;
      gap: 10px;
    }
    button {
      border: 0;
      border-radius: 10px;
      padding: 9px 14px;
      font-size: 13px;
      font-weight: 600;
      cursor: pointer;
      transition: transform .12s ease, opacity .12s ease, box-shadow .12s ease;
    }
    button:hover:not(:disabled) { transform: translateY(-1px); }
    button:active:not(:disabled) { transform: translateY(0); }
    button:disabled { opacity: .6; cursor: not-allowed; }
    .primary {
      color: #fff;
      background: linear-gradient(180deg, #3f81ff, var(--accent));
      box-shadow: 0 8px 16px rgba(30, 99, 233, .24);
    }
    @media (prefers-color-scheme: dark) {
      .primary {
        background: linear-gradient(180deg, #ffffff, #dddddd);
        color: #080808;
        box-shadow: 0 8px 16px rgba(255, 255, 255, .15);
      }
    }
    .secondary {
      color: var(--text);
      background: var(--panel);
      border: 1px solid var(--border);
    }
    .hint {
      margin-top: 12px;
      font-size: 12px;
      color: var(--muted);
      min-height: 18px;
    }
    .content { display: grid; gap: 16px; }
    .task-list {
      width: 100%;
      border-collapse: collapse;
      font-size: 13px;
      min-width: 640px;
    }
    .task-list th, .task-list td {
      padding: 11px 12px;
      border-bottom: 1px solid var(--border);
      text-align: left;
      vertical-align: top;
    }
    .task-list th {
      color: var(--muted);
      font-size: 12px;
      font-weight: 600;
      background: rgba(255, 255, 255, .42);
      position: sticky;
      top: 0;
      z-index: 1;
    }
    .task-row { cursor: pointer; transition: background .15s ease; }
    .task-row:hover { background: rgba(30, 99, 233, .08); }
    .task-row.active { background: rgba(30, 99, 233, .16); }
    @media (prefers-color-scheme: dark) {
      .task-row:hover { background: rgba(255,255,255,.07); }
      .task-row.active { background: rgba(255,255,255,.13); }
      .task-list th { background: rgba(20, 20, 20, .95); }
    }
    .status {
      display: inline-block;
      border-radius: 999px;
      font-size: 11px;
      padding: 3px 8px;
      border: 1px solid transparent;
      white-space: nowrap;
    }
    .status.queued { color: var(--muted); border-color: var(--muted); }
    .status.running { color: var(--warn); border-color: var(--warn); }
    .status.done { color: var(--ok); border-color: var(--ok); }
    .status.error, .status.canceled { color: var(--danger); border-color: var(--danger); }
    .progress {
      display: grid;
      gap: 4px;
      min-width: 180px;
    }
    .progress.compact { min-width: 160px; }
    .progress-track {
      height: 7px;
      border-radius: 999px;
      overflow: hidden;
      border: 1px solid var(--border);
      background: rgba(30, 99, 233, .10);
    }
    .progress-fill {
      height: 100%;
      width: 0;
      background: linear-gradient(90deg, #4f8dff, #1e63e9);
      transition: width .2s ease;
    }
    .progress-line {
      display: flex;
      justify-content: space-between;
      gap: 8px;
      font-size: 11px;
      color: var(--muted);
      line-height: 1.1;
      white-space: nowrap;
    }
    @media (prefers-color-scheme: dark) {
      .progress-track { background: rgba(255,255,255,.12); }
      .progress-fill { background: linear-gradient(90deg, #ffffff, #d8d8d8); }
    }
    .logs {
      margin: 0;
      padding: 13px;
      background: #f7faff;
      border-top: 1px solid var(--border);
      font-family: "Consolas", "SFMono-Regular", Menlo, monospace;
      font-size: 12px;
      line-height: 1.45;
      color: #253455;
      white-space: pre-wrap;
      max-height: 420px;
      overflow: auto;
    }
    @media (prefers-color-scheme: dark) {
      .logs {
        background: #0e0e0e;
        color: #ddd;
      }
    }
    .meta {
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      gap: 8px 12px;
      font-size: 13px;
      color: var(--muted);
    }
    .meta b { color: var(--text); font-weight: 600; display:block; margin-bottom: 4px; font-size: 12px; }
    .meta > div {
      border: 1px solid var(--border);
      border-radius: 10px;
      padding: 8px 10px;
      background: var(--panel);
      min-height: 54px;
    }
    .toolbar {
      display:grid;
      grid-template-columns:185px 1fr;
      gap:10px;
      border-bottom:1px solid var(--border);
      padding:12px 16px;
    }
    .table-wrap {
      overflow: auto;
      max-height: 420px;
    }
    @media (max-width: 980px) {
      .wrap { grid-template-columns: 1fr; }
      .meta { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    }
    @media (max-width: 700px) {
      .wrap { padding: 12px 14px 16px; }
      .checks { grid-template-columns: 1fr; }
      .toolbar { grid-template-columns: 1fr; }
      .meta { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <section class="card">
      <h2>新建下载任务</h2>
      <div class="pad">
        <label for="url">视频/剧集链接</label>
        <input id="url" type="text" placeholder="https://hanime1.me/watch?v=xxxx">

        <label for="outputDir">输出目录</label>
        <input id="outputDir" type="text" value="{{.DefaultOutputDir}}">

        <label for="workDir">工作目录</label>
        <input id="workDir" type="text" value="{{.DefaultWorkDir}}">

        <label for="quality">视频质量</label>
        <select id="quality" data-default-quality="{{.DefaultQuality}}">
          <option value="">自动选择</option>
          <option value="1080p" {{if eq .DefaultQuality "1080p"}}selected{{end}}>1080p</option>
          <option value="720p" {{if eq .DefaultQuality "720p"}}selected{{end}}>720p</option>
          <option value="480p" {{if eq .DefaultQuality "480p"}}selected{{end}}>480p</option>
          <option value="360p" {{if eq .DefaultQuality "360p"}}selected{{end}}>360p</option>
          <option value="240p" {{if eq .DefaultQuality "240p"}}selected{{end}}>240p</option>
          {{if and (ne .DefaultQuality "") (ne .DefaultQuality "1080p") (ne .DefaultQuality "720p") (ne .DefaultQuality "480p") (ne .DefaultQuality "360p") (ne .DefaultQuality "240p")}}
          <option value="{{.DefaultQuality}}" selected>{{.DefaultQuality}}（自定义）</option>
          {{end}}
        </select>

        <label for="retry">重试次数</label>
        <input id="retry" type="number" min="1" max="255" value="{{.DefaultRetry}}">

        <label for="threads">下载线程（1-64）</label>
        <input id="threads" type="number" min="1" max="64" value="{{.DefaultThreads}}">

        <label for="timeoutSec">超时（秒，0 不限制）</label>
        <input id="timeoutSec" type="number" min="0" value="{{.DefaultTimeout}}">

        <label for="logLevel">日志级别</label>
        <select id="logLevel">
          <option value="info" {{if eq .DefaultLogLevel "info"}}selected{{end}}>info</option>
          <option value="debug" {{if eq .DefaultLogLevel "debug"}}selected{{end}}>debug</option>
          <option value="warn" {{if eq .DefaultLogLevel "warn"}}selected{{end}}>warn</option>
          <option value="error" {{if eq .DefaultLogLevel "error"}}selected{{end}}>error</option>
          <option value="fatal" {{if eq .DefaultLogLevel "fatal"}}selected{{end}}>fatal</option>
        </select>

        <div class="checks">
          <label class="check"><input id="info" type="checkbox" {{if .DefaultInfo}}checked{{end}}>仅获取信息</label>
          <label class="check"><input id="series" type="checkbox" {{if .DefaultSeries}}checked{{end}}>下载全集</label>
          <label class="check"><input id="lowQuality" type="checkbox" {{if .DefaultLowQ}}checked{{end}}>最低清晰度</label>
          <label class="check"><input id="notifyDesktop" type="checkbox">桌面通知</label>
          <label class="check"><input id="notifySound" type="checkbox" checked>声音提醒</label>
        </div>

        <div class="actions" style="margin-top:10px;">
          <button id="notifPermBtn" class="secondary" type="button">启用通知权限</button>
        </div>

        <div class="actions">
          <button id="startBtn" class="primary" type="button">开始下载</button>
          <button id="cancelBtn" class="secondary" type="button" disabled>取消选中</button>
          <button id="retryBtn" class="secondary" type="button" disabled>重试选中</button>
          <button id="clearBtn" class="secondary" type="button">清理已结束</button>
        </div>
        <div id="hint" class="hint">就绪。当前最大并发任务数：{{.MaxJobs}}</div>
      </div>
    </section>

    <section class="content">
      <article class="card">
        <h2>任务列表</h2>
        <div class="toolbar">
          <select id="statusFilter">
            <option value="all">全部状态</option>
            <option value="queued">排队中</option>
            <option value="running">下载中</option>
            <option value="done">已完成</option>
            <option value="error">失败</option>
            <option value="canceled">已取消</option>
          </select>
          <input id="searchFilter" type="text" placeholder="搜索：链接 / 输出目录 / 质量 / 状态">
        </div>
        <div class="table-wrap">
          <table class="task-list">
            <thead>
              <tr>
                <th style="width:80px">ID</th>
                <th>链接</th>
                <th style="width:210px">进度</th>
                <th style="width:120px">状态</th>
                <th style="width:170px">创建时间</th>
              </tr>
            </thead>
            <tbody id="taskRows"></tbody>
          </table>
        </div>
      </article>

      <article class="card">
        <h2>任务详情</h2>
        <div class="pad">
          <div id="meta" class="meta"><div><b>提示</b>请选择一个任务查看详情</div></div>
        </div>
        <pre id="logs" class="logs">尚未选择任务。</pre>
      </article>
    </section>
  </div>

  <script>
    const ui = {
      rows: document.getElementById("taskRows"),
      hint: document.getElementById("hint"),
      logs: document.getElementById("logs"),
      meta: document.getElementById("meta"),
      startBtn: document.getElementById("startBtn"),
      cancelBtn: document.getElementById("cancelBtn"),
      retryBtn: document.getElementById("retryBtn"),
      clearBtn: document.getElementById("clearBtn"),
      notifPermBtn: document.getElementById("notifPermBtn"),
      url: document.getElementById("url"),
      outputDir: document.getElementById("outputDir"),
      workDir: document.getElementById("workDir"),
      quality: document.getElementById("quality"),
      retry: document.getElementById("retry"),
      threads: document.getElementById("threads"),
      timeoutSec: document.getElementById("timeoutSec"),
      info: document.getElementById("info"),
      series: document.getElementById("series"),
      lowQuality: document.getElementById("lowQuality"),
      logLevel: document.getElementById("logLevel"),
      statusFilter: document.getElementById("statusFilter"),
      searchFilter: document.getElementById("searchFilter"),
      notifyDesktop: document.getElementById("notifyDesktop"),
      notifySound: document.getElementById("notifySound")
    };

    const statusText = {
      queued: "排队中",
      running: "下载中",
      done: "已完成",
      error: "失败",
      canceled: "已取消"
    };

    let selectedTaskId = null;
    let tasks = [];
    let pollTimer = null;
    const taskStateMemo = new Map();

    function statusLabel(v) {
      return statusText[v] || v || "-";
    }

    function progressStageLabel(stage) {
      const map = {
        queued: "排队中",
        downloading: "下载中",
        retrying: "重试中",
        merging: "合并中",
        complete: "已完成",
        error: "失败",
        canceled: "已取消"
      };
      return map[stage] || stage || "";
    }

    function setHint(msg, isError = false) {
      ui.hint.textContent = msg || "";
      ui.hint.style.color = isError ? "var(--danger)" : "var(--muted)";
    }

    function esc(s) {
      return (s || "")
        .replaceAll("&", "&amp;")
        .replaceAll("<", "&lt;")
        .replaceAll(">", "&gt;");
    }

    function fmtTime(v) {
      if (!v) return "-";
      const d = new Date(v);
      if (Number.isNaN(d.getTime())) return "-";
      return d.toLocaleString();
    }

    function progressPercent(task) {
      const p = Number(task && task.progress);
      if (!Number.isFinite(p)) return 0;
      const bounded = Math.max(0, Math.min(1, p));
      return Math.round(bounded * 100);
    }

    function progressMeta(task) {
      if (!task) return "";
      const parts = [];
      const stage = progressStageLabel(task.progressStage);
      if (stage) parts.push(stage);
      if (task.progressSpeed && task.progressSpeed > 0) parts.push(formatSpeed(task.progressSpeed));
      if (task.progressRemainingSec && task.progressRemainingSec > 0) parts.push("ETA " + task.progressRemainingSec + "s");
      return parts.join(" · ");
    }

    function formatSpeed(bytesPerSec) {
      if (!bytesPerSec || bytesPerSec <= 0) return "";
      const units = ["B/s", "KB/s", "MB/s", "GB/s"];
      let size = Number(bytesPerSec);
      let idx = 0;
      while (size >= 1024 && idx < units.length - 1) {
        size /= 1024;
        idx++;
      }
      const fixed = idx === 0 ? size.toFixed(0) : size.toFixed(1);
      return fixed + " " + units[idx];
    }

    function progressBarHTML(task, compact = false) {
      const pct = progressPercent(task);
      const meta = esc(progressMeta(task));
      const cls = compact ? "progress compact" : "progress";
      return '<div class="' + cls + '">' +
        '<div class="progress-track"><div class="progress-fill" style="width:' + pct + '%"></div></div>' +
        '<div class="progress-line"><span>' + pct + '%</span><span>' + meta + '</span></div>' +
      '</div>';
    }

    function updateCancelButton(task) {
      ui.cancelBtn.disabled = !(task && (task.status === "queued" || task.status === "running"));
      ui.retryBtn.disabled = !(task && (task.status === "error" || task.status === "canceled" || task.status === "done"));
    }

    function ensureQualityOption() {
      const known = new Set(["", "1080p", "720p", "480p", "360p", "240p"]);
      const def = (ui.quality.getAttribute("data-default-quality") || "").trim();
      if (!def) {
        ui.quality.value = "";
        return;
      }
      if (!known.has(def)) {
        const custom = document.createElement("option");
        custom.value = def;
        custom.textContent = def + "（自定义）";
        ui.quality.appendChild(custom);
      }
      ui.quality.value = def;
    }

    function loadClientPrefs() {
      try {
        const raw = localStorage.getItem("hani-webui-prefs");
        if (!raw) return;
        const p = JSON.parse(raw);
        if (typeof p.statusFilter === "string") ui.statusFilter.value = p.statusFilter;
        if (typeof p.searchFilter === "string") ui.searchFilter.value = p.searchFilter;
        if (typeof p.notifyDesktop === "boolean") ui.notifyDesktop.checked = p.notifyDesktop;
        if (typeof p.notifySound === "boolean") ui.notifySound.checked = p.notifySound;
      } catch (_) {}
    }

    function saveClientPrefs() {
      const p = {
        statusFilter: ui.statusFilter.value,
        searchFilter: ui.searchFilter.value,
        notifyDesktop: ui.notifyDesktop.checked,
        notifySound: ui.notifySound.checked
      };
      localStorage.setItem("hani-webui-prefs", JSON.stringify(p));
    }

    function getVisibleTasks() {
      const status = ui.statusFilter.value;
      const keyword = (ui.searchFilter.value || "").trim().toLowerCase();
      return tasks.filter(t => {
        if (status !== "all" && t.status !== status) return false;
        if (!keyword) return true;
        const hay = [t.url, t.outputDir, t.workDir, t.quality, t.status, statusLabel(t.status)].join(" ").toLowerCase();
        return hay.includes(keyword);
      });
    }

    function playBeep(freq) {
      try {
        const Ctx = window.AudioContext || window.webkitAudioContext;
        if (!Ctx) return;
        const ctx = new Ctx();
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.type = "sine";
        osc.frequency.value = freq;
        gain.gain.value = 0.03;
        osc.connect(gain);
        gain.connect(ctx.destination);
        osc.start();
        setTimeout(() => { osc.stop(); ctx.close(); }, 220);
      } catch (_) {}
    }

    function sendDesktopNotification(title, body) {
      if (!ui.notifyDesktop.checked) return;
      if (!("Notification" in window)) return;
      if (Notification.permission === "granted") {
        new Notification(title, { body: body });
      }
    }

    function notifyTaskTransition(task, prevStatus) {
      const now = task.status;
      if (prevStatus === now) return;
      if (now !== "done" && now !== "error" && now !== "canceled") return;
      if (!prevStatus) return;

      const title = "任务 #" + task.id + " " + statusLabel(now);
      const body = task.url || "";
      sendDesktopNotification(title, body);
      if (ui.notifySound.checked) {
        playBeep(now === "done" ? 880 : 260);
      }
    }

	    function renderTaskRows() {
	      const visible = getVisibleTasks();
	      if (!visible.length) {
	        ui.rows.innerHTML = '<tr><td colspan="5" style="padding:14px;color:var(--muted)">当前筛选条件下无任务</td></tr>';
	        return;
	      }

	      ui.rows.innerHTML = visible.map(t => {
	        const active = t.id === selectedTaskId ? "active" : "";
	        return '<tr class="task-row ' + active + '" data-id="' + t.id + '">' +
	          '<td>' + t.id + '</td>' +
	          '<td title="' + esc(t.url) + '">' + esc(t.url) + '</td>' +
	          '<td>' + progressBarHTML(t, true) + '</td>' +
	          '<td><span class="status ' + t.status + '">' + esc(statusLabel(t.status)) + '</span></td>' +
	          '<td>' + fmtTime(t.createdAt) + '</td>' +
	          '</tr>';
	      }).join("");

      for (const row of ui.rows.querySelectorAll(".task-row")) {
        row.addEventListener("click", () => {
          selectedTaskId = Number(row.dataset.id);
          renderTaskRows();
          refreshTaskDetail();
        });
      }
    }

    async function fetchTasks() {
      const res = await fetch("/api/tasks");
      if (!res.ok) throw new Error("获取任务列表失败");
      const data = await res.json();
      const nextTasks = Array.isArray(data.tasks) ? data.tasks : [];
      const existing = new Set(nextTasks.map(t => t.id));
      for (const t of nextTasks) {
        const prev = taskStateMemo.get(t.id);
        notifyTaskTransition(t, prev);
        taskStateMemo.set(t.id, t.status);
      }
      for (const id of Array.from(taskStateMemo.keys())) {
        if (!existing.has(id)) taskStateMemo.delete(id);
      }
      tasks = nextTasks;
      if (!selectedTaskId && tasks.length) {
        selectedTaskId = tasks[0].id;
      } else if (selectedTaskId && !tasks.some(t => t.id === selectedTaskId)) {
        selectedTaskId = tasks.length ? tasks[0].id : null;
      }
      renderTaskRows();
    }

    function renderTaskDetail(task) {
      if (!task) {
        ui.meta.innerHTML = "<div><b>提示</b>请选择一个任务查看详情</div>";
        ui.logs.textContent = "尚未选择任务。";
        updateCancelButton(null);
        return;
      }

	      ui.meta.innerHTML =
	        '<div><b>ID</b> ' + task.id + '</div>' +
	        '<div><b>状态</b> <span class="status ' + task.status + '">' + statusLabel(task.status) + '</span></div>' +
          '<div><b>进度阶段</b> ' + esc(progressStageLabel(task.progressStage) || "-") + '</div>' +
          '<div style="grid-column: span 3;"><b>任务进度</b>' + progressBarHTML(task, false) + '</div>' +
          '<div><b>链接</b> ' + esc(task.url) + '</div>' +
          '<div><b>输出目录</b> ' + esc(task.outputDir || "默认") + '</div>' +
          '<div><b>工作目录</b> ' + esc(task.workDir || "默认") + '</div>' +
          '<div><b>质量</b> ' + esc(task.quality || "自动") + '</div>' +
          '<div><b>重试次数</b> ' + task.retry + '</div>' +
          '<div><b>线程数</b> ' + (task.threads || 20) + '</div>' +
          '<div><b>超时</b> ' + (task.timeoutSec || 0) + 's</div>' +
          '<div><b>仅信息</b> ' + (task.info ? "是" : "否") + '</div>' +
	        '<div><b>下载全集</b> ' + (task.series ? "是" : "否") + '</div>' +
	        '<div><b>最低清晰度</b> ' + (task.lowQuality ? "是" : "否") + '</div>' +
	        '<div><b>日志级别</b> ' + esc(task.logLevel || "info") + '</div>' +
	        '<div><b>创建时间</b> ' + fmtTime(task.createdAt) + '</div>' +
	        '<div><b>开始时间</b> ' + fmtTime(task.startedAt) + '</div>' +
	        '<div><b>结束时间</b> ' + fmtTime(task.finishedAt) + '</div>';

      const lines = Array.isArray(task.logs) ? task.logs : [];
      ui.logs.textContent = lines.length ? lines.join("\n") : "暂无日志输出。";
      ui.logs.scrollTop = ui.logs.scrollHeight;
      updateCancelButton(task);
    }

    async function refreshTaskDetail() {
      if (!selectedTaskId) {
        renderTaskDetail(null);
        return;
      }
	      const res = await fetch("/api/tasks/" + selectedTaskId);
      if (!res.ok) {
        renderTaskDetail(null);
        return;
      }
      const task = await res.json();
      renderTaskDetail(task);
    }

    async function createTask() {
      const body = {
        url: ui.url.value.trim(),
        outputDir: ui.outputDir.value.trim(),
        workDir: ui.workDir.value.trim(),
        quality: ui.quality.value.trim(),
        retry: Number(ui.retry.value) || 10,
        threads: Number(ui.threads.value) || 20,
        timeoutSec: Number(ui.timeoutSec.value) || 0,
        info: ui.info.checked,
        series: ui.series.checked,
        lowQuality: ui.lowQuality.checked,
        logLevel: ui.logLevel.value
      };

      if (!body.url) {
        setHint("请输入下载链接。", true);
        return;
      }
      if (body.threads < 1 || body.threads > 64) {
        setHint("线程数必须在 1 到 64 之间。", true);
        return;
      }

      ui.startBtn.disabled = true;
      setHint("正在创建任务...");
      try {
        const res = await fetch("/api/tasks", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body)
        });
        if (!res.ok) {
          const text = await res.text();
          throw new Error(text || "创建任务失败");
        }
        const task = await res.json();
        selectedTaskId = task.id;
        await fetchTasks();
        await refreshTaskDetail();
	        setHint("任务 #" + task.id + " 已创建。");
      } catch (err) {
        setHint(err.message || String(err), true);
      } finally {
        ui.startBtn.disabled = false;
      }
    }

    async function cancelSelectedTask() {
      if (!selectedTaskId) return;
      ui.cancelBtn.disabled = true;
	      setHint("正在取消任务 #" + selectedTaskId + "...");
	      try {
	        const res = await fetch("/api/tasks/" + selectedTaskId + "/cancel", { method: "POST" });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.message || "取消失败");
        setHint(data.message || "已发出取消请求");
      } catch (err) {
        setHint(err.message || String(err), true);
      } finally {
        ui.cancelBtn.disabled = false;
      }
    }

    async function retrySelectedTask() {
      if (!selectedTaskId) return;
      ui.retryBtn.disabled = true;
      setHint("正在重试任务 #" + selectedTaskId + "...");
      try {
        const res = await fetch("/api/tasks/" + selectedTaskId + "/retry", { method: "POST" });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.message || "重试失败");
        const task = data.task || null;
        if (task && task.id) {
          selectedTaskId = task.id;
          await fetchTasks();
          await refreshTaskDetail();
          setHint("任务 #" + task.id + " 已重新创建。");
        } else {
          await tick();
          setHint("重试请求已提交。");
        }
      } catch (err) {
        setHint(err.message || String(err), true);
      } finally {
        ui.retryBtn.disabled = false;
      }
    }

    async function clearFinishedTasks() {
      ui.clearBtn.disabled = true;
      try {
        const res = await fetch("/api/tasks/finished", { method: "DELETE" });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error("清理失败");
        setHint("已清理 " + (data.deleted || 0) + " 个已结束任务。");
        await tick();
      } catch (err) {
        setHint(err.message || String(err), true);
      } finally {
        ui.clearBtn.disabled = false;
      }
    }

    async function requestNotificationPermission() {
      if (!("Notification" in window)) {
        setHint("当前浏览器不支持桌面通知。", true);
        return;
      }
      const perm = await Notification.requestPermission();
      if (perm === "granted") {
        setHint("通知权限已开启。");
      } else {
        setHint("通知权限状态：" + perm, true);
      }
    }

    async function tick() {
      try {
        await fetchTasks();
        await refreshTaskDetail();
      } catch (err) {
        setHint(err.message || String(err), true);
      }
    }

    ui.startBtn.addEventListener("click", createTask);
    ui.cancelBtn.addEventListener("click", cancelSelectedTask);
    ui.retryBtn.addEventListener("click", retrySelectedTask);
    ui.clearBtn.addEventListener("click", clearFinishedTasks);
    ui.notifPermBtn.addEventListener("click", requestNotificationPermission);
    ui.statusFilter.addEventListener("change", () => { saveClientPrefs(); renderTaskRows(); });
    ui.searchFilter.addEventListener("input", () => { saveClientPrefs(); renderTaskRows(); });
    ui.notifyDesktop.addEventListener("change", saveClientPrefs);
    ui.notifySound.addEventListener("change", saveClientPrefs);

    ensureQualityOption();
    loadClientPrefs();
    tick();
    pollTimer = setInterval(tick, 1500);
    window.addEventListener("beforeunload", () => clearInterval(pollTimer));
  </script>
</body>
</html>`
