package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/server/middleware"
)

// adminHTTPClient returns an HTTP client for calls to the gateway's admin API (/api/v1). When an
// admin token is configured (LILTOK_ADMIN_TOKEN or server.admin_token in the config file), it is
// sent on every /api/v1 request, so CLI commands and the MCP bridge keep working once the gateway
// requires it.
func adminHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: adminTokenTransport{base: http.DefaultTransport}}
}

type adminTokenTransport struct {
	base http.RoundTripper
}

func (t adminTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.Path, "/api/v1/") && req.Header.Get(middleware.AdminTokenHeader) == "" {
		if token := configuredAdminToken(); token != "" {
			req = req.Clone(req.Context())
			req.Header.Set(middleware.AdminTokenHeader, token)
		}
	}
	return t.base.RoundTrip(req)
}

// configuredAdminToken returns the admin token from the environment or the config file.
func configuredAdminToken() string {
	if v := os.Getenv("LILTOK_ADMIN_TOKEN"); v != "" {
		return v
	}
	if path := resolveConfigPath(configPath); path != "" {
		if cfg, err := config.Load(path); err == nil {
			return cfg.Server.AdminToken
		}
	}
	return ""
}
