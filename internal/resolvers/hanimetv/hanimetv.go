package hanimetv

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/acgtools/hanime-hunter/internal/resolvers"
	"github.com/acgtools/hanime-hunter/pkg/util"
	"github.com/charmbracelet/log"
	"golang.org/x/net/html"
)

func init() {
	resolvers.Resolvers.Register("hanime.tv", New())
}

func New() resolvers.Resolver {
	return &resolver{}
}

var _ resolvers.Resolver = (*resolver)(nil)

type resolver struct{}

const videoAPIURL = "https://hanime.tv/api/v8/video?id="

func (re *resolver) Resolve(u string, opt *resolvers.Option) ([]*resolvers.HAnime, error) {
	if strings.Contains(u, "playlists") {
		return resolvePlaylist(u)
	}

	urlRes, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("解析链接 %q 失败: %w", u, err)
	}
	site := urlRes.Host

	slug, err := getVideoID(urlRes.Path)
	if err != nil {
		return nil, err
	}

	v, err := getVideoInfo(slug)
	if err != nil {
		return nil, err
	}

	log.Infof("已找到视频：%s，正在搜索剧集，请稍候...", v.HentaiFranchise.Title)

	res := make([]*resolvers.HAnime, 0)
	episodes := make([]string, 0)

	if !opt.Series {
		vidMap, eps := getVidMap(v)

		episodes = append(episodes, eps[0])
		log.Infof("找到剧集：%#q", episodes)

		res = append(res, &resolvers.HAnime{
			URL:    u,
			Site:   site,
			Title:  v.HentaiFranchise.Title,
			Videos: vidMap,
		})

		return res, nil
	}

	for _, fv := range v.HentaiFranchiseHentaiVideos {
		video, err := getVideoInfo(fv.Slug)
		if err != nil {
			return nil, err
		}

		vidMap, eps := getVidMap(video)
		episodes = append(episodes, eps[0])
		res = append(res, &resolvers.HAnime{
			Site:   site,
			Title:  video.HentaiFranchise.Slug,
			Videos: vidMap,
		})
	}

	log.Infof("找到剧集：%#q", episodes)

	return res, nil
}

func resolvePlaylist(u string) ([]*resolvers.HAnime, error) {
	slugs, err := getPlaylistSlugs(u)
	if err != nil {
		return nil, err
	}

	res := make([]*resolvers.HAnime, 0)

	for _, s := range slugs {
		v, err := getVideoInfo(s)
		if err != nil {
			return nil, err
		}

		log.Infof("已找到视频：%s，正在搜索剧集，请稍候...", v.HentaiFranchise.Title)

		vidMap, eps := getVidMap(v)

		log.Infof("找到剧集：%#q", eps[0])

		res = append(res, &resolvers.HAnime{
			Title:  v.HentaiFranchise.Title,
			Videos: vidMap,
		})

		time.Sleep(time.Duration(util.RandomInt63n(900, 3000)) * time.Millisecond) //nolint:gomnd
	}

	return res, nil
}

func getPlaylistSlugs(u string) ([]string, error) {
	doc, err := util.GetHTMLPage(NewClient(), u, map[string]string{"User-Agent": resolvers.UA})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	listDivs := util.FindTagByNameAttrs(doc, "div", true, []html.Attribute{{Key: "class", Val: "playlists__panel panel__content"}})
	if len(listDivs) == 0 {
		return nil, fmt.Errorf("在 %q 中未找到播放列表", u)
	}

	aTags := util.FindTagByNameAttrs(listDivs[0], "a", true, []html.Attribute{{Key: "class", Val: "flex row"}})
	if len(aTags) == 0 {
		return nil, fmt.Errorf("在 %q 中未找到视频", u)
	}

	res := make([]string, 0)
	for _, a := range aTags {
		href := util.GetAttrVal(a, "href")
		urlRes, _ := url.Parse(href)
		path := urlRes.Path
		if !strings.HasPrefix(path, "/videos/hentai/") {
			continue
		}
		res = append(res, strings.TrimPrefix(path, "/videos/hentai/"))
	}

	return res, nil
}

func getVidMap(v *Video) (map[string]*resolvers.Video, []string) {
	vidMap := make(map[string]*resolvers.Video)
	eps := make([]string, 0)

	for _, s := range v.VideosManifest.Servers[0].Streams {
		if s.Height == "1080" {
			continue
		}
		quality := s.Height + "p"

		eps = append(eps, v.HentaiVideo.Slug)
		log.Debugf("找到视频分辨率: %s %s", v.HentaiVideo.Slug, quality)

		vidMap[quality] = &resolvers.Video{
			ID:      strconv.FormatInt(s.ID, 10),
			Quality: quality,
			URL:     s.URL,
			IsM3U8:  true,
			Title:   v.HentaiVideo.Slug,
			Size:    s.Size,
			Ext:     "mp4",
		}
	}

	return vidMap, eps
}

func getVideoID(path string) (string, error) {
	if !strings.HasPrefix(path, "/videos/hentai/") {
		return "", fmt.Errorf("在 %q 中未找到视频 ID", path)
	}

	params := strings.Split(path, "/")
	if len(params) != 4 { //nolint:gomnd
		return "", fmt.Errorf("在 %q 中未找到视频 ID", path)
	}

	return params[3], nil
}

func getVideoInfo(slug string) (*Video, error) {
	resp, err := util.Get(NewClient(), fmt.Sprintf("%s%s", videoAPIURL, slug), map[string]string{"User-Agent": resolvers.UA})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取视频 %q 响应失败: %w", slug, err)
	}

	v := &Video{}
	if err := json.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("解析视频 %q 的 JSON 响应失败: %w", slug, err)
	}

	return v, nil
}

var sharedClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout: 30 * time.Second, //nolint:gomnd
		DisableKeepAlives:   false,
		Proxy:               util.ProxyFromEnvOrSystem,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     96,
		IdleConnTimeout:     90 * time.Second,
	},
	Timeout: 15 * time.Minute, //nolint:gomnd
}

func NewClient() *http.Client {
	return sharedClient
}
