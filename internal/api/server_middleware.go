package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/gin-gonic/gin"
	codexlive "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/live"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/safemode"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

var corsExposedResponseHeaders = []string{
	logging.CPATraceIDHeader,
	"X-CPA-VERSION",
	"X-CPA-COMMIT",
	"X-CPA-BUILD-DATE",
	"X-CPA-SUPPORT-PLUGIN",
	"X-CPA-HOME-VERSION",
	"X-CPA-HOME-BUILD-DATE",
	"X-SERVER-VERSION",
	"X-SERVER-BUILD-DATE",
	"Location",
	"Retry-After",
	"X-Request-Id",
	"OpenAI-Request-Id",
}

// directModelPolicyBodyLimit bounds the request body buffered purely to read a
// model identifier. Realtime/live payloads are small JSON control messages; SDP
// bodies carry no model and fall back to the query/default model.
const directModelPolicyBodyLimit = 1 << 20

const directModelPolicyBodyTooLargeKey = "directModelPolicyBodyTooLarge"

// directModelPolicyMiddleware applies plugin-owned model admission to direct
// live/realtime entry points that do not enter BaseAPIHandler's execution
// interceptor path. Those handlers select an upstream OAuth credential directly,
// so they never reach the request interceptors used by the OpenAI/Claude/Gemini
// paths. The plugin receives the effective model (request model or the surface's
// default) plus an explicit no-reservation marker, because these surfaces do not
// emit the normalized usage records used to settle control-plane reservations.
// The middleware is a no-op when no plugin host is configured; the plugin passes
// through requests whose principal carries no managed-key metadata.
func (s *Server) directModelPolicyMiddleware(defaultModel string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.pluginHost == nil || c == nil || c.Request == nil {
			c.Next()
			return
		}
		// The standard server builder installs a plugin host even when plugins
		// are disabled or none declares a pre-route policy. Do not inspect,
		// buffer, or reject request bodies unless a policy actually exists.
		if !s.pluginHost.HasActivePreRoutePolicy("") {
			c.Next()
			return
		}
		// This legacy sideband spelling addresses an already-created call. It has
		// no request model to evaluate, matching /v1/realtime/calls/:call_id.
		if c.Request.Method == http.MethodGet && strings.TrimSpace(c.Query("call_id")) != "" {
			c.Next()
			return
		}
		model, ok := s.directRequestedModel(c, defaultModel)
		if !ok {
			status := http.StatusBadRequest
			message, code := "Invalid Realtime request body", "invalid_request"
			if tooLarge, _ := c.Get(directModelPolicyBodyTooLargeKey); tooLarge == true {
				status = http.StatusRequestEntityTooLarge
				message, code = "Realtime request body exceeds policy inspection limit", "request_too_large"
			}
			c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
				"message": message,
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    code,
			}})
			return
		}
		metadata := map[string]any{"without_budget_reservation": true}
		if value, exists := c.Get("accessMetadata"); exists {
			if accessMetadata, ok := value.(map[string]string); ok && len(accessMetadata) > 0 {
				metadata["access_metadata"] = accessMetadata
			}
		}
		body := directPolicyBody(c)
		response := s.pluginHost.CheckPreRoutePolicy(c.Request.Context(), pluginapi.RequestInterceptRequest{RequestID: uuid.NewString(), TraceID: logging.GetRequestID(c.Request.Context()), SourceFormat: "realtime", Model: model, RequestedModel: model, Stream: true, Headers: c.Request.Header.Clone(), Body: body, Metadata: metadata})
		if applyDirectPolicyTermination(c, response) {
			return
		}
		c.Next()
	}
}

// applyDirectPolicyTermination relays a plugin termination decision to the
// client. It reports whether the request was terminated (aborting the chain).
func applyDirectPolicyTermination(c *gin.Context, response pluginapi.RequestInterceptResponse) bool {
	if !response.Terminate {
		return false
	}
	for key, values := range response.ResponseHeaders {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	status := response.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusForbidden
	}
	c.Data(status, response.ResponseHeaders.Get("Content-Type"), response.ResponseBody)
	c.Abort()
	return true
}

// directPolicyBody snapshots the (already restored) request body for the
// interceptor payload without consuming it for the downstream handler.
func directPolicyBody(c *gin.Context) []byte {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(c.Request.Body, directModelPolicyBodyLimit+1))
	c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), c.Request.Body))
	return body
}

// directRequestedModel resolves the model a realtime/live request targets and
// restores the body so the downstream handler still reads it in full.
func (s *Server) directRequestedModel(c *gin.Context, defaultModel string) (string, bool) {
	if c.Request == nil || c.Request.Body == nil || c.Request.Method == http.MethodGet {
		if model := strings.TrimSpace(codexlive.ClientSecretSessionModel(directClientSecretSession(c))); model != "" {
			return model, true
		}
		if model := strings.TrimSpace(c.Query("model")); model != "" {
			return model, true
		}
		return defaultModel, true
	}
	body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, directModelPolicyBodyLimit+1))
	if errRead != nil {
		return "", false
	}
	if len(body) > directModelPolicyBodyLimit {
		// Never authorize a default model while the downstream handler can find a
		// different model later in a larger JSON or multipart body.
		c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), c.Request.Body))
		c.Set(directModelPolicyBodyTooLargeKey, true)
		return "", false
	}
	c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), c.Request.Body))
	model, errModel := codexlive.RequestedCallModel(body, c.GetHeader("Content-Type"), directClientSecretSession(c))
	if errModel != nil {
		return "", false
	}
	if strings.TrimSpace(model) != "" {
		return model, true
	}
	return defaultModel, true
}

// directClientSecretSession reads the realtime client-secret session stored by
// realtimeAuthMiddleware, if this request authenticated with one.
func directClientSecretSession(c *gin.Context) json.RawMessage {
	if c == nil {
		return nil
	}
	value, exists := c.Get(codexlive.ClientSecretSessionContextKey)
	if !exists {
		return nil
	}
	session, _ := value.(json.RawMessage)
	return session
}

var corsExposedResponseHeadersJoined = strings.Join(corsExposedResponseHeaders, ", ")

const (
	exampleAPIKeyManagementPath = "/management.html"
	exampleAPIKeyManagementURL  = "/management.html?safe-mode=configure"
)

func (s *Server) homeHeartbeatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil || !s.cfg.Home.Enabled {
			c.Next()
			return
		}
		if c != nil && c.Request != nil {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/v0/management/") || path == "/v0/management" || strings.HasPrefix(path, "/v0/resource/plugins/") || path == "/management.html" {
				c.Next()
				return
			}
		}
		client := home.Current()
		if client == nil || !client.HeartbeatOK() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.Next()
	}
}

func (s *Server) exampleAPIKeySafeModeRequired(cfg *config.Config) bool {
	return s != nil && s.exampleAPIKeySafeModeEnabled && cfg != nil && safemode.HasExampleAPIKeys(cfg.APIKeys)
}

func (s *Server) exampleAPIKeySafeModeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || !s.exampleAPIKeySafeModeActive.Load() || c == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if path == exampleAPIKeyManagementPath && c.Query("safe-mode") == "configure" {
			c.Next()
			return
		}
		if (path == "/" || path == exampleAPIKeyManagementPath) && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			s.serveExampleAPIKeyWarningPage(c)
			return
		}
		if !isExampleAPIKeySafeModeProxyPath(path) {
			c.Next()
			return
		}

		c.Header("X-CPA-SAFE-MODE", "example-api-key")
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":   "unsafe_example_api_key",
			"message": "Proxy API endpoints are disabled because api-keys contains template values. Open /management.html?safe-mode=configure, update api-keys in Management, then retry.",
		})
	}
}

func (s *Server) serveExampleAPIKeyWarningPage(c *gin.Context) {
	cfg := s.cfg
	var keys []string
	if cfg != nil {
		keys = safemode.ExampleAPIKeys(cfg.APIKeys)
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		c.Abort()
		return
	}
	c.String(http.StatusOK, safemode.ExampleAPIKeyWarningPageHTML(keys, exampleAPIKeyManagementURL))
	c.Abort()
}

func isExampleAPIKeySafeModeProxyPath(path string) bool {
	switch {
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		return true
	case path == "/v1beta" || strings.HasPrefix(path, "/v1beta/"):
		return true
	case path == "/openai/v1" || strings.HasPrefix(path, "/openai/v1/"):
		return true
	case path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/"):
		return true
	default:
		return false
	}
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
//
// Returns:
//   - gin.HandlerFunc: The CORS middleware handler
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Access-Control-Expose-Headers", corsExposedResponseHeadersJoined)

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// AuthMiddleware returns a Gin middleware handler that authenticates requests
// using the configured authentication providers. When no providers are available,
// it allows all requests (legacy behaviour).
func AuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return accessAuthMiddleware(manager, false)
}

func realtimeStandardAuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return accessAuthMiddleware(manager, true)
}

func accessAuthMiddleware(manager *sdkaccess.Manager, realtimeError bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			c.Next()
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result != nil {
				c.Set("userApiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
			}
			c.Next()
			return
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		if realtimeError {
			errorType := "authentication_error"
			code := "invalid_api_key"
			if statusCode >= http.StatusInternalServerError {
				errorType = "server_error"
				code = "authentication_service_error"
			}
			c.AbortWithStatusJSON(statusCode, gin.H{"error": gin.H{
				"message": err.Message,
				"type":    errorType,
				"param":   nil,
				"code":    code,
			}})
			return
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}

func realtimeAuthMiddleware(manager *sdkaccess.Manager, handler *codexlive.Handler) gin.HandlerFunc {
	fallback := realtimeStandardAuthMiddleware(manager)
	return func(c *gin.Context) {
		authorization, matched, errAuthenticate := handler.AuthenticateClientSecret(c.Request)
		if !matched {
			fallback(c)
			return
		}
		if errAuthenticate != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
				"message": errAuthenticate.Error(),
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    "invalid_realtime_client_secret",
			}})
			return
		}
		principal := authorization.IssuerPrincipal
		if principal == "" {
			principal = authorization.Principal
		}
		provider := authorization.IssuerProvider
		if provider == "" {
			provider = "realtime-client-secret"
		}
		c.Set("userApiKey", principal)
		c.Set("accessProvider", provider)
		c.Set(codexlive.ClientSecretSessionContextKey, authorization.Session)
		c.Set(codexlive.ClientSecretPrincipalContextKey, authorization.Principal)
		c.Next()
	}
}
