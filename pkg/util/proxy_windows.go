//go:build windows

package util

import (
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	regInternetSettings = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
)

func proxyFromEnvOrSystem(req *http.Request) (*url.URL, error) {
	if p, err := http.ProxyFromEnvironment(req); p != nil || err != nil {
		return p, err
	}

	proxyServer, enabled, err := readWindowsSystemProxy()
	if err != nil || !enabled || strings.TrimSpace(proxyServer) == "" {
		return nil, nil
	}

	addr := selectProxyServer(proxyServer, req.URL.Scheme)
	if strings.TrimSpace(addr) == "" {
		return nil, nil
	}

	return normalizeProxyURL(addr)
}

func readWindowsSystemProxy() (proxyServer string, enabled bool, err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, regInternetSettings, registry.QUERY_VALUE)
	if err != nil {
		return "", false, err
	}
	defer key.Close()

	enabledVal, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil {
		return "", false, err
	}

	proxyServer, _, err = key.GetStringValue("ProxyServer")
	if err != nil {
		return "", enabledVal == 1, err
	}

	return proxyServer, enabledVal == 1, nil
}

func selectProxyServer(proxyServer, scheme string) string {
	if !strings.Contains(proxyServer, "=") {
		return strings.TrimSpace(proxyServer)
	}

	var fallback string
	entries := strings.Split(proxyServer, ";")
	for _, entry := range entries {
		parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		if val == "" {
			continue
		}

		if key == "socks" && fallback == "" {
			fallback = "socks5://" + val
		}
		if key == strings.ToLower(scheme) {
			return val
		}
	}

	return fallback
}

func normalizeProxyURL(addr string) (*url.URL, error) {
	v := strings.TrimSpace(addr)
	if v == "" {
		return nil, nil
	}

	if !strings.Contains(v, "://") {
		v = "http://" + v
	}

	return url.Parse(v) //nolint:wrapcheck
}
