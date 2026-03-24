//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"github.com/acgtools/hanime-hunter/internal/request"
	"github.com/acgtools/hanime-hunter/internal/resolvers"
	"github.com/acgtools/hanime-hunter/pkg/util"
	"github.com/lxn/walk"
	"golang.org/x/net/html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type videoPreview struct {
	URL          string
	Site         string
	Title        string
	Qualities    []string
	EpisodeCount int
	ThumbnailURL string
	Image        image.Image
	ImageNote    string
}

func (app *desktopGUI) applyDefaultPreviewState() {
	app.setPreviewIdle("输入链接后可预览缩略图与可用清晰度。")
}

func (app *desktopGUI) onPreviewURLChanged() {
	current := strings.TrimSpace(app.urlEdit.Text())
	app.stopAutoPreviewTimer()

	app.previewMu.RLock()
	curPreview := app.preview
	app.previewMu.RUnlock()

	if curPreview != nil && strings.TrimSpace(curPreview.URL) == current {
		return
	}

	app.previewSeq.Add(1)
	app.setPreviewIdle("输入链接后可预览缩略图与可用清晰度。")
	if current == "" {
		return
	}
	if !app.currentSettingsSnapshot().AutoPreview {
		return
	}

	series := false
	if app.seriesCheck != nil {
		series = app.seriesCheck.Checked()
	}
	app.scheduleAutoPreview(current, series)
}

func (app *desktopGUI) previewCurrentURL() {
	u := strings.TrimSpace(app.urlEdit.Text())
	if u == "" {
		app.showInfo("视频预览", "请先输入视频链接。")
		return
	}

	app.showSideTab(sideTabPreview)
	app.startPreview(u, app.seriesCheck.Checked(), true)
}

func (app *desktopGUI) maybeWarmPreview(u string) {
	series := false
	if app.seriesCheck != nil {
		series = app.seriesCheck.Checked()
	}
	app.maybeWarmPreviewWithSeries(u, series)
}

func (app *desktopGUI) maybeWarmPreviewWithSeries(u string, series bool) {
	url := strings.TrimSpace(u)
	if url == "" {
		return
	}

	app.previewMu.RLock()
	curPreview := app.preview
	app.previewMu.RUnlock()

	if curPreview != nil && strings.TrimSpace(curPreview.URL) == url {
		return
	}

	app.startPreview(url, series, false)
}

func (app *desktopGUI) scheduleAutoPreview(u string, series bool) {
	app.autoPreviewMu.Lock()
	defer app.autoPreviewMu.Unlock()

	if app.autoPreviewTimer != nil {
		app.autoPreviewTimer.Stop()
	}
	app.autoPreviewTimer = time.AfterFunc(450*time.Millisecond, func() {
		app.maybeWarmPreviewWithSeries(u, series)
	})
}
func (app *desktopGUI) startPreview(u string, series bool, surfaceErrors bool) {
	seq := app.previewSeq.Add(1)

	app.queueUI(func() {
		if app.previewBtn != nil {
			app.previewBtn.SetEnabled(false)
		}
		app.setPreviewIdle("正在加载视频预览，请稍候...")
	})

	go func(previewSeq int64, url string, seriesMode bool, reportErrors bool) {
		preview, err := resolveVideoPreview(url, seriesMode)

		app.queueUI(func() {
			if app.previewSeq.Load() != previewSeq {
				return
			}
			if app.previewBtn != nil {
				app.previewBtn.SetEnabled(true)
			}
			if err != nil {
				app.setPreviewError(url, err)
				if reportErrors {
					app.showError("加载预览失败", err)
				}
				return
			}
			app.applyPreview(preview)
		})
	}(seq, u, series, surfaceErrors)
}

func (app *desktopGUI) applyPreview(preview *videoPreview) {
	if preview == nil {
		app.setPreviewIdle("当前没有可显示的预览。")
		return
	}

	app.previewMu.Lock()
	app.preview = preview
	app.previewMu.Unlock()

	app.disposePreviewBitmap()

	if preview.Image != nil {
		bmp, err := walk.NewBitmapFromImage(preview.Image)
		if err == nil {
			app.previewBitmap = bmp
			var img walk.Image = bmp
			_ = app.previewImage.SetImage(img)
		} else {
			preview.ImageNote = "缩略图显示失败: " + err.Error()
			app.clearPreviewImage()
		}
	} else {
		app.clearPreviewImage()
	}

	_ = app.previewStatusLabel.SetText("预览已更新: " + safePreviewTitle(preview.Title))
	_ = app.previewInfoEdit.SetText(formatPreviewText(preview))
}

func (app *desktopGUI) setPreviewIdle(status string) {
	app.previewMu.Lock()
	app.preview = nil
	app.previewMu.Unlock()

	app.disposePreviewBitmap()
	app.clearPreviewImage()

	if app.previewStatusLabel != nil {
		_ = app.previewStatusLabel.SetText(status)
	}
	if app.previewInfoEdit != nil {
		_ = app.previewInfoEdit.SetText("")
	}
}

func (app *desktopGUI) setPreviewError(url string, err error) {
	app.previewMu.Lock()
	app.preview = nil
	app.previewMu.Unlock()

	app.disposePreviewBitmap()
	app.clearPreviewImage()

	if app.previewStatusLabel != nil {
		_ = app.previewStatusLabel.SetText("预览加载失败")
	}
	if app.previewInfoEdit != nil {
		lines := []string{
			"链接: " + url,
		}
		if err != nil {
			lines = append(lines, "错误: "+err.Error())
		}
		_ = app.previewInfoEdit.SetText(strings.Join(lines, "\r\n"))
	}
}

func (app *desktopGUI) clearPreviewImage() {
	if app.previewImage == nil {
		return
	}
	var img walk.Image
	_ = app.previewImage.SetImage(img)
}

func (app *desktopGUI) disposePreviewBitmap() {
	if app.previewBitmap != nil {
		app.previewBitmap.Dispose()
		app.previewBitmap = nil
	}
}

func resolveVideoPreview(u string, series bool) (*videoPreview, error) {
	anis, err := resolvers.Resolve(u, &resolvers.Option{Series: series})
	if err != nil {
		return nil, err
	}
	if len(anis) == 0 {
		return nil, errors.New("未获取到可预览的视频信息")
	}

	first := anis[0]
	preview := &videoPreview{
		URL:          strings.TrimSpace(u),
		Site:         strings.TrimSpace(first.Site),
		Title:        strings.TrimSpace(first.Title),
		Qualities:    collectPreviewQualities(first.Videos),
		EpisodeCount: len(anis),
	}

	if preview.Title == "" {
		preview.Title = firstAvailableVideoTitle(first.Videos)
	}
	if preview.Title == "" {
		preview.Title = "未命名视频"
	}

	thumbURL, img, imgErr := loadPreviewThumbnail(u)
	preview.ThumbnailURL = thumbURL
	preview.Image = img
	if imgErr != nil {
		preview.ImageNote = imgErr.Error()
	}

	return preview, nil
}

func collectPreviewQualities(videos map[string]*resolvers.Video) []string {
	if len(videos) == 0 {
		return nil
	}

	res := make([]string, 0, len(videos))
	for quality := range videos {
		quality = strings.TrimSpace(quality)
		if quality == "" {
			continue
		}
		res = append(res, quality)
	}

	sort.SliceStable(res, func(i, j int) bool {
		ri := previewQualityRank(res[i])
		rj := previewQualityRank(res[j])
		if ri == rj {
			return res[i] < res[j]
		}
		return ri > rj
	})

	return res
}

func previewQualityRank(q string) int {
	v := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(q)), "p")
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

func firstAvailableVideoTitle(videos map[string]*resolvers.Video) string {
	for _, video := range videos {
		if video == nil {
			continue
		}
		title := strings.TrimSpace(video.Title)
		if title != "" {
			return title
		}
	}
	return ""
}

func loadPreviewThumbnail(pageURL string) (string, image.Image, error) {
	thumbURL, err := discoverThumbnailURL(pageURL)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(thumbURL) == "" {
		return "", nil, errors.New("未找到页面缩略图")
	}

	resp, err := request.Request(http.MethodGet, thumbURL, map[string]string{
		"Accept":  "image/avif,image/apng,image/*,*/*;q=0.8",
		"Referer": pageURL,
	})
	if err != nil {
		return thumbURL, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return thumbURL, nil, fmt.Errorf("缩略图请求失败: HTTP %d", resp.StatusCode)
	}

	img, _, err := image.Decode(resp.Body)
	if err != nil {
		return thumbURL, nil, fmt.Errorf("解析缩略图失败: %w", err)
	}

	return thumbURL, img, nil
}

func discoverThumbnailURL(pageURL string) (string, error) {
	resp, err := request.Request(http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("预览页面请求失败: HTTP %d", resp.StatusCode)
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return "", fmt.Errorf("解析预览页面失败: %w", err)
	}

	metaTags := util.FindTagByNameAttrs(doc, "meta", false, nil)
	for _, candidate := range []struct {
		key string
		val string
	}{
		{key: "property", val: "og:image"},
		{key: "property", val: "og:image:url"},
		{key: "name", val: "twitter:image"},
		{key: "name", val: "twitter:image:src"},
		{key: "itemprop", val: "image"},
	} {
		if content := findMetaContent(metaTags, candidate.key, candidate.val); content != "" {
			return resolvePreviewURL(pageURL, content), nil
		}
	}

	linkTags := util.FindTagByNameAttrs(doc, "link", false, nil)
	for _, rel := range []string{"image_src", "thumbnail", "preload"} {
		if href := findLinkHref(linkTags, rel); href != "" {
			return resolvePreviewURL(pageURL, href), nil
		}
	}

	return "", nil
}

func findMetaContent(metaTags []*html.Node, key, expected string) string {
	expected = strings.ToLower(strings.TrimSpace(expected))
	for _, node := range metaTags {
		if strings.ToLower(strings.TrimSpace(util.GetAttrVal(node, key))) != expected {
			continue
		}
		content := strings.TrimSpace(util.GetAttrVal(node, "content"))
		if content != "" {
			return content
		}
	}
	return ""
}

func findLinkHref(linkTags []*html.Node, expectedRel string) string {
	expectedRel = strings.ToLower(strings.TrimSpace(expectedRel))
	for _, node := range linkTags {
		rel := strings.ToLower(strings.TrimSpace(util.GetAttrVal(node, "rel")))
		if rel == "" || !strings.Contains(rel, expectedRel) {
			continue
		}
		href := strings.TrimSpace(util.GetAttrVal(node, "href"))
		if href != "" {
			return href
		}
	}
	return ""
}

func resolvePreviewURL(pageURL, raw string) string {
	base, err := neturl.Parse(pageURL)
	if err != nil {
		return raw
	}
	ref, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	return base.ResolveReference(ref).String()
}

func formatPreviewText(preview *videoPreview) string {
	if preview == nil {
		return ""
	}

	lines := []string{
		"标题: " + safePreviewTitle(preview.Title),
		"站点: " + safePreviewText(preview.Site),
	}

	if preview.EpisodeCount > 1 {
		lines = append(lines, fmt.Sprintf("下载模式: 整季/全集（共 %d 集）", preview.EpisodeCount))
	} else {
		lines = append(lines, "下载模式: 单视频")
	}

	if len(preview.Qualities) > 0 {
		lines = append(lines, "可用清晰度: "+strings.Join(preview.Qualities, " / "))
	} else {
		lines = append(lines, "可用清晰度: 未识别")
	}

	if strings.TrimSpace(preview.ThumbnailURL) != "" {
		lines = append(lines, "缩略图来源: "+preview.ThumbnailURL)
	}
	if strings.TrimSpace(preview.ImageNote) != "" {
		lines = append(lines, "缩略图提示: "+preview.ImageNote)
	}

	return strings.Join(lines, "\r\n")
}

func safePreviewTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "未命名视频"
	}
	return s
}

func safePreviewText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}
