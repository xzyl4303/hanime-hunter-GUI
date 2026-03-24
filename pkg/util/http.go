package util

import (
	"fmt"
	"net/http"

	"golang.org/x/net/html"
)

func Get(client *http.Client, u string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("为 %q 创建请求失败: %w", u, err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("向 %q 发送请求失败: %w", u, err)
	}

	return resp, nil
}

func GetHTMLPage(client *http.Client, u string, headers map[string]string) (*html.Node, error) {
	resp, err := Get(client, u, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析 %q 的 HTML 失败: %w", u, err)
	}

	return doc, nil
}
