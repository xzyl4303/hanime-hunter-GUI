//go:build !windows

package util

import (
	"net/http"
	"net/url"
)

func proxyFromEnvOrSystem(req *http.Request) (*url.URL, error) {
	return http.ProxyFromEnvironment(req)
}
