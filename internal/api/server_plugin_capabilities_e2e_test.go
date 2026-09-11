package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestPluginCapabilitiesEndToEnd loads a real plugin shared library and asserts
// the capabilities added by this change are wired end to end through the host:
//
//   - PublicAPI: the plugin-owned surface is mounted under /v0/control-plane and
//     dispatched over public.handle, without inheriting management auth;
//   - the management panel injects a plugin-served /v0/control-plane/ui.js;
//   - FrontendAuthProvider: the plugin is consulted for credentials it claims,
//     and an unknown one is refused rather than silently accepted;
//   - configured static API keys keep working, so a governance plugin cannot
//     break existing deployments.
//
// The test is plugin-agnostic: any library implementing the capability contract
// satisfies it. It is opt-in because it needs a compiled plugin binary.
//
//	CLIPROXY_PLUGIN_E2E_SO=/path/to/plugin.so \
//	CLIPROXY_PLUGIN_E2E_ID=control-plane \
//	go test ./internal/api/ -run TestPluginCapabilitiesEndToEnd -v -count=1
func TestPluginCapabilitiesEndToEnd(t *testing.T) {
	soPath := strings.TrimSpace(os.Getenv("CLIPROXY_PLUGIN_E2E_SO"))
	if soPath == "" {
		t.Skip("CLIPROXY_PLUGIN_E2E_SO is not set")
	}
	pluginID := strings.TrimSpace(os.Getenv("CLIPROXY_PLUGIN_E2E_ID"))
	if pluginID == "" {
		pluginID = "control-plane"
	}
	if _, errStat := os.Stat(soPath); errStat != nil {
		t.Fatalf("plugin binary %q unavailable: %v", soPath, errStat)
	}

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("create auth dir: %v", errMkdir)
	}
	// A plugin that persists state resolves its data directory from the auth dir,
	// so keep it inside the test's temporary tree.
	t.Setenv("CLIPROXY_AUTH_DIR", authDir)

	pluginsDir := filepath.Join(tmpDir, "plugins")
	if errMkdir := os.MkdirAll(pluginsDir, 0o755); errMkdir != nil {
		t.Fatalf("create plugins dir: %v", errMkdir)
	}
	if errStage := stagePluginBinary(soPath, filepath.Join(pluginsDir, pluginID+".so")); errStage != nil {
		t.Fatalf("stage plugin binary: %v", errStage)
	}

	configYAML := fmt.Sprintf(`
port: 0
auth-dir: %q
api-keys:
  - static-test-key
plugins:
  enabled: true
  dir: %q
  configs:
    %s:
      enabled: true
      priority: 1
      google_client_id: "e2e-dummy-client-id"
      google_client_secret: "e2e-dummy-client-secret"
      google_redirect_url: "http://127.0.0.1:1/oauth/callback"
      cookie_secure: false
      session_ttl: "30m"
      state_ttl: "5m"
      allowed_email_domains:
        - example.test
`, authDir, pluginsDir, pluginID)

	configPath := filepath.Join(tmpDir, "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(configYAML), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	cfg, errLoad := proxyconfig.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("load config: %v", errLoad)
	}

	host := pluginhost.New()
	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()
	server := NewServer(cfg, authManager, accessManager, configPath, WithPluginHost(host))

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelLoad()
	host.ApplyConfig(loadCtx, cfg)
	if !host.PluginLoaded(pluginID) {
		t.Fatalf("plugin %q did not load", pluginID)
	}
	// Mirror the production plugin sync sequence: providers and plugin-owned
	// routes are wired after the plugin loads.
	host.RegisterFrontendAuthProviders()
	accessManager.SetProviders(sdkaccess.RegisteredProviders())
	host.RegisterUsagePlugins()
	server.RefreshPluginManagementRoutes()

	do := func(method, target, key, body string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, reader)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		return rr
	}

	t.Run("PublicRouteIsMountedAndPluginOwned", func(t *testing.T) {
		// 401 proves the request reached the plugin and the plugin's own
		// authentication answered. A 404 would mean the mount is missing; a 200
		// would mean the surface is unauthenticated.
		rr := do(http.MethodGet, "/v0/control-plane/organizations", "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated public route status = %d, want 401 body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("PublicRouteDoesNotInheritManagementAuth", func(t *testing.T) {
		// The operator's management credential must not unlock a plugin-owned
		// surface; only the plugin's own session may.
		rr := do(http.MethodGet, "/v0/control-plane/organizations", "static-test-key", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("static key on public route status = %d, want 401 body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("ManagementPanelInjectsPluginUI", func(t *testing.T) {
		if !host.PublicRouteServed(http.MethodGet, "/v0/control-plane/ui.js") {
			t.Fatal("plugin does not serve /v0/control-plane/ui.js")
		}
		rr := do(http.MethodGet, "/v0/control-plane/ui.js", "", "")
		if rr.Code != http.StatusOK || rr.Body.Len() == 0 {
			t.Fatalf("ui.js status = %d len = %d, want a served asset", rr.Code, rr.Body.Len())
		}
	})

	t.Run("UnknownPluginCredentialIsRefused", func(t *testing.T) {
		// The plugin claims the cpa_ prefix, so an unknown one must be rejected
		// rather than falling through to another provider.
		rr := do(http.MethodGet, "/v1/models", "cpa_definitely-not-a-real-key", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("unknown managed key status = %d, want 401 body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("StaticAPIKeysAreUnaffected", func(t *testing.T) {
		// A governance plugin must never break credentials it does not own.
		rr := do(http.MethodGet, "/v1/models", "static-test-key", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("static key status = %d, want 200 body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("MissingCredentialIsStillRefused", func(t *testing.T) {
		rr := do(http.MethodGet, "/v1/models", "", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous status = %d, want 401", rr.Code)
		}
	})
}

func stagePluginBinary(source, destination string) error {
	data, errRead := os.ReadFile(source)
	if errRead != nil {
		return errRead
	}
	return os.WriteFile(destination, data, 0o755)
}
