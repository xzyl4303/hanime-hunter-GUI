package downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/acgtools/hanime-hunter/internal/request"
	"github.com/acgtools/hanime-hunter/internal/resolvers"
	"github.com/acgtools/hanime-hunter/internal/resolvers/hanimetv"
	"github.com/acgtools/hanime-hunter/internal/tui/color"
	"github.com/acgtools/hanime-hunter/internal/tui/progressbar"
	"github.com/acgtools/hanime-hunter/pkg/util"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"
	"github.com/grafov/m3u8"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

const (
	defaultGoRoutineNum = 20
	maxGoRoutineNum     = 64

	retrySleepMinMS = 250
	retrySleepMaxMS = 900
)

type Downloader struct {
	p      *tea.Program
	Option *Option
}

type ProgressEvent struct {
	FileName   string
	Ratio      float64
	Status     string
	Speed      int64
	RemainingS float64
	Downloaded int64
	Total      int64
}

type Option struct {
	OutputDir        string
	Quality          string
	Info             bool
	LowQuality       bool
	Retry            uint8
	Threads          uint8
	ProgressCallback func(ProgressEvent)
}

func NewDownloader(p *tea.Program, opt *Option) *Downloader {
	return &Downloader{
		p:      p,
		Option: opt,
	}
}

func (d *Downloader) Download(ani *resolvers.HAnime, m *progressbar.Model) error {
	videos := resolvers.SortAniVideos(ani.Videos, d.Option.LowQuality)

	if d.Option.Info {
		log.Infof("可用视频如下：\n%s", sPrintVideosInfo(videos))
		return nil
	}

	video := videos[0] // by default, download the highest quality
	if d.Option.Quality != "" {
		if v, ok := ani.Videos[strings.ToLower(d.Option.Quality)]; ok {
			video = v
		}
	}

	err := d.save(video, ani.Title, m)
	if err != nil {
		return fmt.Errorf("下载文件 %q 失败: %w", video.Title, err)
	}

	return nil
}

func (d *Downloader) SendPbStatus(fileName, status string) {
	if d.p != nil {
		d.p.Send(progressbar.ProgressStatusMsg{
			FileName: fileName,
			Status:   status,
		})
	}
	d.emitProgress(ProgressEvent{
		FileName: fileName,
		Status:   normalizeStatus(status),
	})
}

func (d *Downloader) SendPbProgress(fileName string, ratio float64) {
	if d.p != nil {
		d.p.Send(progressbar.ProgressMsg{
			FileName: fileName,
			Ratio:    ratio,
		})
	}
	d.emitProgress(ProgressEvent{
		FileName: fileName,
		Ratio:    clampRatio(ratio),
	})
}

func (d *Downloader) emitProgress(evt ProgressEvent) {
	if d == nil || d.Option == nil || d.Option.ProgressCallback == nil {
		return
	}
	d.Option.ProgressCallback(evt)
}

func normalizeStatus(status string) string {
	switch status {
	case progressbar.DownloadingStatus:
		return "downloading"
	case progressbar.MergingStatus:
		return "merging"
	case progressbar.CompleteStatus:
		return "complete"
	case progressbar.RetryStatus:
		return "retrying"
	case progressbar.ErrStatus:
		return "error"
	default:
		return ""
	}
}

func clampRatio(ratio float64) float64 {
	switch {
	case ratio < 0:
		return 0
	case ratio > 1:
		return 1
	default:
		return ratio
	}
}

func sPrintVideosInfo(vs []*resolvers.Video) string {
	var sb strings.Builder
	for _, v := range vs {
		sb.WriteString(fmt.Sprintf(" 标题: %s, 清晰度: %s, 格式: %s\n", v.Title, v.Quality, v.Ext))
	}

	return sb.String()
}

func (d *Downloader) save(v *resolvers.Video, aniTitle string, m *progressbar.Model) error {
	outputDir := filepath.Join(d.Option.OutputDir, aniTitle)
	err := os.MkdirAll(outputDir, os.ModePerm)
	if err != nil {
		if d.p != nil {
			d.p.Send(progressbar.ProgressErrMsg{Err: err})
		}
		return fmt.Errorf("创建下载目录 %q 失败: %w", outputDir, err)
	}

	fName := fmt.Sprintf("%s %s.%s", v.Title, v.Quality, v.Ext)
	fPath := filepath.Join(outputDir, fName)
	if f, err := os.Lstat(fPath); err == nil {
		if f.Size() == v.Size || v.IsM3U8 {
			log.Infof("文件 %q 已存在，跳过", fPath)
			return nil
		}
	}

	if !v.IsM3U8 {
		return d.saveSingleVideo(v, fPath, fName, m)
	}

	return d.saveM3U8(v, outputDir, fPath, fName, m)
}

func (d *Downloader) saveSingleVideo(v *resolvers.Video, fPath, fName string, m *progressbar.Model) error {
	pb := progressBar(d.p, v.Size, fName, func(fileName string, ratio, dltime float64, speed int64) {
		d.emitProgress(ProgressEvent{
			FileName:   fileName,
			Ratio:      clampRatio(ratio),
			Status:     "downloading",
			Speed:      speed,
			RemainingS: dltime,
			Downloaded: int64(float64(v.Size) * clampRatio(ratio)),
			Total:      v.Size,
		})
	})
	if m != nil {
		m.AddPb(pb)
	}

	file, err := os.OpenFile(fPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gomnd
	if err != nil {
		d.SendPbStatus(fName, progressbar.ErrStatus)
		return fmt.Errorf("创建文件 %q 失败: %w", fPath, err)
	}
	defer file.Close()

	fStat, err := file.Stat()
	if err != nil {
		d.SendPbStatus(fName, progressbar.ErrStatus)
		return fmt.Errorf("读取文件状态 %q 失败: %w", fPath, err)
	}

	curSize := fStat.Size()
	if curSize > 0 {
		pb.Pw.Downloaded = curSize
		d.SendPbProgress(fName, float64(curSize)/float64(v.Size))
		d.emitProgress(ProgressEvent{
			FileName:   fName,
			Ratio:      clampRatio(float64(curSize) / float64(v.Size)),
			Status:     "downloading",
			Downloaded: curSize,
			Total:      v.Size,
		})
	}

	headers := map[string]string{}
	for i := 1; ; i++ {
		headers["Range"] = fmt.Sprintf("bytes=%d-", curSize)
		written, err := writeFile(d.p, pb.Pw, file, v.URL, headers)
		if err == nil {
			break
		} else if i-1 == int(d.Option.Retry) {
			d.SendPbStatus(fName, progressbar.ErrStatus)
			return err
		}

		curSize += written
		d.SendPbStatus(fName, progressbar.RetryStatus)

		time.Sleep(time.Duration(util.RandomInt63n(retrySleepMinMS, retrySleepMaxMS)) * time.Millisecond)
	}

	d.SendPbStatus(fName, progressbar.CompleteStatus)

	return nil
}

func writeFile(p *tea.Program, pw *progressbar.ProgressWriter, file *os.File, u string, headers map[string]string) (int64, error) {
	resp, err := request.Request(http.MethodGet, u, headers)
	if err != nil {
		return 0, fmt.Errorf("发送请求到 %q 失败: %w", u, err)
	}
	defer resp.Body.Close()

	pw.File = file
	pw.Reader = resp.Body
	if p == nil {
		written, err := io.Copy(pw.File, io.TeeReader(pw.Reader, pw))
		if err != nil {
			return written, err //nolint:wrapcheck
		}
		return written, nil
	}
	return pw.Start(p) //nolint:wrapcheck
}

func (d *Downloader) saveM3U8(v *resolvers.Video, outputDir, fPath, fName string, m *progressbar.Model) error {
	segURIs, mediaPL, err := getSegURIs(v.URL)
	if err != nil {
		return err
	}

	tmpDir := filepath.Join(outputDir, "tmp-"+fName)
	err = os.MkdirAll(tmpDir, os.ModePerm)
	if err != nil {
		return fmt.Errorf("创建目录 %q 失败: %w", tmpDir, err)
	}
	defer os.RemoveAll(tmpDir)

	fileListPath := filepath.Join(tmpDir, "fileList.txt")
	err = createTmpFileList(fileListPath, len(segURIs))
	if err != nil {
		return err
	}

	key, iv, err := getKeyIV(mediaPL)
	if err != nil {
		return err
	}

	totalSegs := int64(len(segURIs))
	pb := countProgressBar(d.p, totalSegs, fName, func(fileName string, ratio float64, downloaded, total int64) {
		d.emitProgress(ProgressEvent{
			FileName:   fileName,
			Ratio:      clampRatio(ratio),
			Status:     "downloading",
			Downloaded: downloaded,
			Total:      total,
		})
	})
	if m != nil {
		m.AddPb(pb)
	}

	ctx := context.Background()
	threadNum := int64(defaultGoRoutineNum)
	if d.Option.Threads > 0 {
		threadNum = int64(d.Option.Threads)
	}
	if threadNum > maxGoRoutineNum {
		threadNum = maxGoRoutineNum
	}

	tsClient := &http.Client{
		Transport: &http.Transport{
			Proxy:               util.ProxyFromEnvOrSystem,
			TLSHandshakeTimeout: 30 * time.Second,
			DisableKeepAlives:   false,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: int(threadNum) + 16,
			MaxConnsPerHost:     int(threadNum) + 16,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 15 * time.Minute,
	}

	sem := semaphore.NewWeighted(threadNum)
	group := &errgroup.Group{} // no need to cancel other dl goroutines
	dlTS := func(idx, u string) func() error {
		return func() error {
			if err := sem.Acquire(ctx, 1); err != nil {
				return fmt.Errorf("下载 TS 分片失败: %w", err)
			}
			defer sem.Release(1)

			tsPath := filepath.Join(tmpDir, idx+".ts")
			for i := 1; ; i++ {
				err := saveTS(tsClient, tsPath, u, key, iv)
				if err == nil {
					break
				} else if i-1 == int(d.Option.Retry) {
					return err
				}

				time.Sleep(time.Duration(util.RandomInt63n(retrySleepMinMS, retrySleepMaxMS)) * time.Millisecond)
			}
			pb.Pc.Increase()
			return nil
		}
	}

	for i, u := range segURIs {
		group.Go(dlTS(strconv.Itoa(i), u))
	}
	if err := group.Wait(); err != nil {
		d.SendPbStatus(fName, progressbar.ErrStatus)
		return fmt.Errorf("下载 %s 失败: %w", fName, err)
	}

	return d.mergeFiles(fileListPath, fName, fPath)
}

func (d *Downloader) mergeFiles(fileListPath, fName, fPath string) error {
	d.SendPbStatus(fName, progressbar.MergingStatus)

	err := util.MergeToMP4(fileListPath, fPath)
	if err != nil {
		return fmt.Errorf("合并文件失败: %w", err)
	}

	d.SendPbStatus(fName, progressbar.CompleteStatus)

	return nil
}

func createTmpFileList(path string, num int) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建 %q 失败: %w", path, err)
	}
	defer file.Close()

	for i := 0; i < num; i++ {
		_, err := file.WriteString(fmt.Sprintf("file '%s.ts'\n", strconv.Itoa(i)))
		if err != nil {
			return fmt.Errorf("写入文件列表失败: %w", err)
		}
	}

	return nil
}

func getSegURIs(u string) ([]string, *m3u8.MediaPlaylist, error) {
	m3u8Data, err := getM3U8Data(u)
	if err != nil {
		return nil, nil, err
	}

	list, listType, err := m3u8.DecodeFrom(bytes.NewReader(m3u8Data), true)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 m3u8 数据失败: %w", err)
	}
	if listType != m3u8.MEDIA {
		return nil, nil, errors.New("未找到媒体数据")
	}
	mediaPL := list.(*m3u8.MediaPlaylist) //nolint:forcetypeassert

	segURIs := make([]string, 0)
	for _, s := range mediaPL.Segments {
		if s == nil {
			continue
		}
		segURIs = append(segURIs, s.URI)
	}

	return segURIs, mediaPL, nil
}

func saveTS(client *http.Client, path, u string, key, iv []byte) error {
	headers := map[string]string{
		"User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/39.0.2171.27 Safari/537.36",
	}

	if client == nil {
		client = hanimetv.NewClient()
	}
	resp, err := util.Get(client, u, headers)
	if err != nil {
		return fmt.Errorf("下载 TS 分片失败: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("返回 404，请确认该视频当前是否可在线播放")
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取 %q 数据失败: %w", u, err)
	}

	if len(data) == 0 { // if there is no data here, skip
		return nil
	}

	tsData, err := util.AESDecrypt(data, key, iv)
	if err != nil {
		return fmt.Errorf("解密 %q 数据失败: %w", u, err)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建 %q 失败: %w", path, err)
	}
	defer file.Close()

	_, err = file.Write(tsData)
	if err != nil {
		return fmt.Errorf("写入 %q 失败: %w", path, err)
	}

	return nil
}

func getKeyIV(mediaPL *m3u8.MediaPlaylist) ([]byte, []byte, error) {
	resp, err := request.Request(http.MethodGet, mediaPL.Key.URI, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("获取 m3u8 密钥失败: %w", err)
	}
	defer resp.Body.Close()

	key, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 m3u8 密钥失败: %w", err)
	}

	iv := key
	if mediaPL.Key.IV != "" {
		iv = []byte(mediaPL.Key.IV)
	}

	return key, iv, nil
}

func getM3U8Data(u string) ([]byte, error) {
	client := hanimetv.NewClient()
	headers := map[string]string{
		"User-Agent": resolvers.UA,
	}

	resp, err := util.Get(client, u, headers)
	if err != nil {
		return nil, fmt.Errorf("获取 m3u8 数据失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 m3u8 数据失败: %w", err)
	}

	return data, nil
}

func countProgressBar(
	p *tea.Program,
	total int64,
	fileName string,
	onProgress func(fileName string, ratio float64, downloaded, total int64),
) *progressbar.ProgressBar {
	pc := &progressbar.ProgressCounter{
		Total:      total,
		Downloaded: atomic.Int64{},
		FileName:   fileName,
		Onprogress: func(fileName string, ratio float64) {
			if p != nil {
				p.Send(progressbar.ProgressMsg{
					FileName: fileName,
					Ratio:    ratio,
				})
			}
			if onProgress != nil {
				downloaded := int64(float64(total) * clampRatio(ratio))
				onProgress(fileName, ratio, downloaded, total)
			}
		},
	}

	colors := color.PbColors.Colors()

	pb := &progressbar.ProgressBar{
		Pc:       pc,
		Progress: progress.New(progress.WithGradient(colors[0], colors[1])),
		FileName: fileName,
		Status:   progressbar.DownloadingStatus,
	}

	return pb
}

func progressBar(
	p *tea.Program,
	total int64,
	fileName string,
	onProgress func(fileName string, ratio, dltime float64, speed int64),
) *progressbar.ProgressBar {
	pw := &progressbar.ProgressWriter{
		FileName:  fileName,
		Total:     total,
		StartTime: time.Now(),
		OnProgress: func(fileName string, ratio, dltime float64, speed int64) {
			if p != nil {
				p.Send(progressbar.ProgressMsg{
					FileName: fileName,
					Ratio:    ratio,
					DLTime:   dltime,
					Speed:    speed,
				})
			}
			if onProgress != nil {
				onProgress(fileName, ratio, dltime, speed)
			}
		},
	}

	colors := color.PbColors.Colors()

	pb := &progressbar.ProgressBar{
		Pw:       pw,
		Progress: progress.New(progress.WithGradient(colors[0], colors[1])),
		FileName: fileName,
		Status:   progressbar.DownloadingStatus,
	}

	return pb
}
