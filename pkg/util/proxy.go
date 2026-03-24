package util

import (
	"net/http"
	"net/url"
)

// ProxyFromEnvOrSystem prefers environment proxy settings.
// If none are set, it falls back to OS-level proxy settings when supported.
func ProxyFromEnvOrSystem(req *http.Request) (*url.URL, error) {
	return proxyFromEnvOrSystem(req)
}
