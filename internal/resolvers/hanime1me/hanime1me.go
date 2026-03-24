package hanime1me

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/acgtools/hanime-hunter/internal/resolvers"
	"github.com/acgtools/hanime-hunter/pkg/util"
	"github.com/charmbracelet/log"
	"golang.org/x/net/html"
)

const (
	defaultAniTitle  = "unknown"
	pageFetchRetries = 3
	dlInfoRetries    = 4
)

func init() {
	resolvers.Resolvers.Register("hanime1.me", New())
}

func New() resolvers.Resolver {
	return &resolver{}
}

var _ resolvers.Resolver = (*resolver)(nil)

type resolver struct{}

func (re *resolver) Resolve(u string, opt *resolvers.Option) ([]*resolvers.HAnime, error) {
	if strings.Contains(u, "playlist") {
		return resolvePlaylist(u)
	}

	site, vid, err := getSiteAndVID(u)
	if err != nil {
		return nil, fmt.Errorf("解析链接 %q 失败: %w", u, err)
	}

	title, series, err := getAniInfo(u)
	if err != nil {
		return nil, fmt.Errorf("获取视频信息失败（%q）: %w", u, err)
	}

	if title == defaultAniTitle {
		log.Warn("获取视频标题失败")
	}
	log.Infof("已找到视频：%s，正在搜索剧集，请稍候...", title)

	res := make([]*resolvers.HAnime, 0)

	if !opt.Series {
		videos, eps, err := getDLInfoWithRetry(vid)
		if err != nil {
			return nil, fmt.Errorf("获取下载信息失败（%q）: %w", vid, err)
		}

		if len(eps) > 0 {
			log.Infof("找到剧集：%q", eps[0])
		}

		res = append(res, &resolvers.HAnime{
			URL:    u,
			Site:   site,
			Title:  title,
			Videos: videos,
		})

		return res, nil
	}

	titles := make([]string, 0)
	for _, s := range series {
		_, vID, _ := getSiteAndVID(s)
		videos, eps, err := getDLInfoWithRetry(vID)
		if err != nil {
			return nil, fmt.Errorf("获取下载信息失败（%q）: %w", vID, err)
		}

		if len(eps) > 0 {
			titles = append(titles, eps[0])
		}

		res = append(res, &resolvers.HAnime{
			URL:    s,
			Site:   site,
			Title:  title,
			Videos: videos,
		})
	}

	log.Infof("找到剧集 %#q", titles)

	return res, nil
}

func removeDirInvalidSymbol(title string) string {
	return util.ReplaceChars(title, util.InvalidDirSymbols[:])
}

func resolvePlaylist(u string) ([]*resolvers.HAnime, error) {
	doc, err := getHTMLPageWithRetry(u)
	if err != nil {
		return nil, err
	}

	playlist := util.FindTagByNameAttrs(doc, "div", true, []html.Attribute{{Key: "id", Val: "home-rows-wrapper"}})
	if len(playlist) == 0 {
		return nil, fmt.Errorf("在 %q 中未找到播放列表", u)
	}

	aTags := util.FindTagByNameAttrs(playlist[0], "a", true, []html.Attribute{{Key: "class", Val: "playlist-show-links"}})

	res := make([]*resolvers.HAnime, 0)
	for _, a := range aTags {
		href := util.GetAttrVal(a, "href")
		if strings.Contains(href, "watch") {
			site, vid, err := getSiteAndVID(href)
			if err != nil {
				return nil, err
			}

			title, _, err := getAniInfo(href)
			if err != nil {
				return nil, err
			}

			log.Infof("已找到视频：%s，正在搜索剧集，请稍候...", title)

			videos, eps, err := getDLInfoWithRetry(vid)
			if err != nil {
				return nil, err
			}

			if len(eps) > 0 {
				log.Infof("找到剧集：%#q", eps[0])
			}

			time.Sleep(time.Duration(util.RandomInt63n(900, 3000)) * time.Millisecond)

			res = append(res, &resolvers.HAnime{
				URL:    href,
				Site:   site,
				Title:  title,
				Videos: videos,
			})
		}
	}

	return res, nil
}

func getSiteAndVID(u string) (string, string, error) {
	urlRes, err := url.Parse(u)
	if err != nil {
		return "", "", fmt.Errorf("解析链接 %q 失败: %w", u, err)
	}

	vid, ok := urlRes.Query()["v"]
	if !ok || len(vid) == 0 {
		return "", "", errors.New("未找到视频 ID（参数 v）")
	}

	return urlRes.Host, vid[0], nil
}

func getAniInfo(u string) (string, []string, error) {
	doc, err := getHTMLPageWithRetry(u)
	if err != nil {
		return "", nil, fmt.Errorf("获取视频页面 %q 失败: %w", u, err)
	}

	series := util.FindTagByNameAttrs(doc, "div", true, []html.Attribute{{Key: "id", Val: "video-playlist-wrapper"}})
	if len(series) == 0 {
		return "", nil, fmt.Errorf("获取剧集信息失败（%q）", u)
	}

	seriesTag := series[0]

	title := defaultAniTitle
	titleTag := util.FindTagByNameAttrs(seriesTag, "h4", false, nil)
	if len(titleTag) > 0 {
		title = normalizeTitle(textContent(titleTag[0]))
	}
	if title != defaultAniTitle {
		title = removeDirInvalidSymbol(title)
	}

	return title, getSeriesLinks(seriesTag), nil
}

func getSeriesLinks(node *html.Node) []string {
	list := util.FindTagByNameAttrs(node, "div", true, []html.Attribute{{Key: "id", Val: "playlist-scroll"}})
	if len(list) == 0 {
		return nil
	}

	aTags := util.FindTagByNameAttrs(list[0], "a", false, nil)

	links := make([]string, 0)
	for _, a := range aTags {
		href := util.GetAttrVal(a, "href")
		if strings.Contains(href, "watch") {
			links = append(links, href)
		}
	}

	return links
}

func getDLInfo(vid string) (map[string]*resolvers.Video, []string, error) {
	u := "https://hanime1.me/download?v=" + vid
	doc, err := getHTMLPageWithRetry(u)
	if err != nil {
		return nil, nil, fmt.Errorf("获取下载页面失败: %w", err)
	}

	tables := util.FindTagByNameAttrs(doc, "table", true, []html.Attribute{{Key: "class", Val: "download-table"}})
	if len(tables) == 0 {
		return nil, nil, errors.New("未找到下载信息")
	}

	vidMap := make(map[string]*resolvers.Video)
	episodes := make([]string, 0)

	aTags := util.FindTagByNameAttrs(tables[0], "a", false, nil)
	for _, a := range aTags {
		link := extractDownloadLink(a)
		if link == "" {
			continue
		}

		title := normalizeTitle(removeDirInvalidSymbol(util.GetAttrVal(a, "download")))

		id := getID(link)
		if id == "" {
			id = fmt.Sprintf("video-%d", len(vidMap)+1)
		}

		quality := ""
		if tmp := strings.Split(id, "-"); len(tmp) > 1 {
			quality = tmp[1]
		}
		if quality == "" {
			if m := regexp.MustCompile(`(\d{3,4}p)`).FindStringSubmatch(link); len(m) > 1 {
				quality = m[1]
			} else {
				quality = fmt.Sprintf("unknown-%d", len(vidMap)+1)
			}
		}
		if title == "" {
			title = id
		}

		size, ext, err := getVideoInfo(link)
		if err != nil {
			log.Debugf("跳过不可用下载链接: %q, err=%v", link, err)
			continue
		}
		if ext == "octet-stream" {
			ext = "mp4"
		}

		episodes = append(episodes, title)
		log.Debugf("找到视频: %s - %s - %s", title, quality, ext)

		vidMap[quality] = &resolvers.Video{
			ID:      id,
			Quality: quality,
			URL:     link,
			Title:   title,
			Size:    size,
			Ext:     ext,
		}
	}

	if len(vidMap) == 0 {
		return nil, nil, errors.New("未找到可用下载链接，可能需要登录或页面结构已变更")
	}

	return vidMap, episodes, nil
}

func getDLInfoWithRetry(vid string) (map[string]*resolvers.Video, []string, error) {
	var (
		videos map[string]*resolvers.Video
		eps    []string
		err    error
	)

	for i := 0; i < dlInfoRetries; i++ {
		videos, eps, err = getDLInfo(vid)
		if err == nil {
			return videos, eps, nil
		}
		time.Sleep(time.Duration(util.RandomInt63n(800, 1800)) * time.Millisecond)
	}

	return nil, nil, err
}

func getVideoInfo(u string) (int64, string, error) {
	if strings.TrimSpace(u) == "" {
		return 0, "", errors.New("下载链接为空")
	}

	urlRes, err := url.Parse(u)
	if err != nil || urlRes.Scheme == "" || urlRes.Host == "" {
		return 0, "", fmt.Errorf("下载链接格式无效: %q", u)
	}

	var (
		resp     *http.Response
		fetchErr error
	)

	for i := 0; i < pageFetchRetries; i++ {
		resp, fetchErr = util.Get(newClient(), u, map[string]string{"User-Agent": resolvers.UA})
		if fetchErr == nil {
			break
		}
		time.Sleep(time.Duration(util.RandomInt63n(500, 1200)) * time.Millisecond)
	}

	if fetchErr != nil {
		return 0, "", fmt.Errorf("从 %q 获取下载信息失败: %w", u, fetchErr)
	}
	defer resp.Body.Close()

	ext := "mp4"
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		if parts := strings.Split(contentType, "/"); len(parts) > 1 {
			ext = strings.Split(parts[1], ";")[0]
		}
	}

	size := int64(0)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if parsed, parseErr := strconv.ParseInt(cl, 10, 64); parseErr == nil {
			size = parsed
		}
	}

	return size, ext, nil
}

func getHTMLPageWithRetry(u string) (*html.Node, error) {
	var (
		doc *html.Node
		err error
	)

	for i := 0; i < pageFetchRetries; i++ {
		doc, err = util.GetHTMLPage(newClient(), u, map[string]string{"User-Agent": resolvers.UA})
		if err == nil {
			return doc, nil
		}
		time.Sleep(time.Duration(util.RandomInt63n(500, 1200)) * time.Millisecond)
	}

	return nil, err
}

func extractDownloadLink(a *html.Node) string {
	candidates := []string{
		util.GetAttrVal(a, "href"),
		util.GetAttrVal(a, "data-href"),
		util.GetAttrVal(a, "data-url"),
		util.GetAttrVal(a, "data-download"),
		util.GetAttrVal(a, "data-src"),
	}

	if onclick := util.GetAttrVal(a, "onclick"); onclick != "" {
		if m := regexp.MustCompile(`['"]((?:https?:)?//[^'"]+|/[^'"]+)['"]`).FindStringSubmatch(onclick); len(m) > 1 {
			candidates = append(candidates, m[1])
		}
	}

	for _, c := range candidates {
		if u := normalizeDownloadLink(c); u != "" {
			return u
		}
	}

	return ""
}

func normalizeDownloadLink(link string) string {
	link = strings.TrimSpace(link)
	if link == "" || link == "#" {
		return ""
	}

	lower := strings.ToLower(link)
	if strings.HasPrefix(lower, "javascript:") {
		return ""
	}

	switch {
	case strings.HasPrefix(link, "//"):
		return "https:" + link
	case strings.HasPrefix(link, "/"):
		return "https://hanime1.me" + link
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return link
	case strings.Contains(link, "/") || strings.Contains(link, "?"):
		return "https://hanime1.me/" + strings.TrimLeft(link, "/")
	default:
		return ""
	}
}

func normalizeTitle(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func textContent(n *html.Node) string {
	if n == nil {
		return ""
	}

	var b strings.Builder
	var walk func(*html.Node)

	walk = func(node *html.Node) {
		if node == nil {
			return
		}
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}

	walk(n)

	return b.String()
}

func getID(link string) string {
	r := regexp.MustCompile(`[^/]+-\d+p`)
	return r.FindString(link)
}

var sharedClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout: 30 * time.Second,
		DisableKeepAlives:   false,
		Proxy:               util.ProxyFromEnvOrSystem,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     96,
		IdleConnTimeout:     90 * time.Second,
	},
	Timeout: 15 * time.Minute,
}

func newClient() *http.Client {
	return sharedClient
}
