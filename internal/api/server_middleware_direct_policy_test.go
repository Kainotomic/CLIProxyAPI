package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	codexlive "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/live"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func directPolicyTestContext(t *testing.T, method, target string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	return c, recorder
}

func TestDirectRequestedModelIgnoresQueryModelOnPost(t *testing.T) {
	// The downstream handler routes by the body model and never consults the
	// query string on POST, so policy must evaluate the body model too.
	server := &Server{}
	c, _ := directPolicyTestContext(t, http.MethodPost, "/v1/live?model=gpt-live-1-codex", []byte(`{"model":"other"}`))
	model, ok := server.directRequestedModel(c, "default-model")
	if !ok || model != "other" {
		t.Fatalf("model = %q ok = %v, want body model", model, ok)
	}
	rest, _ := io.ReadAll(c.Request.Body)
	if string(rest) != `{"model":"other"}` {
		t.Fatalf("body not restored: %q", rest)
	}
}

func TestDirectRequestedModelUsesQueryModelOnGet(t *testing.T) {
	server := &Server{}
	c, _ := directPolicyTestContext(t, http.MethodGet, "/v1/realtime?model=gpt-realtime-custom", nil)
	model, ok := server.directRequestedModel(c, "default-model")
	if !ok || model != "gpt-realtime-custom" {
		t.Fatalf("model = %q ok = %v, want query model", model, ok)
	}
}

func TestDirectRequestedModelUsesBodyModelThenSessionModel(t *testing.T) {
	server := &Server{}
	c, _ := directPolicyTestContext(t, http.MethodPost, "/v1/live", []byte(`{"model":"body-model"}`))
	if model, ok := server.directRequestedModel(c, "default-model"); !ok || model != "body-model" {
		t.Fatalf("model = %q ok = %v, want body-model", model, ok)
	}
	c, _ = directPolicyTestContext(t, http.MethodPost, "/v1/live", []byte(`{"session":{"model":"session-model"}}`))
	if model, ok := server.directRequestedModel(c, "default-model"); !ok || model != "session-model" {
		t.Fatalf("model = %q ok = %v, want session-model", model, ok)
	}
}

func TestDirectRequestedModelFallsBackToDefault(t *testing.T) {
	server := &Server{}
	c, _ := directPolicyTestContext(t, http.MethodGet, "/v1/realtime", nil)
	if model, ok := server.directRequestedModel(c, "default-model"); !ok || model != "default-model" {
		t.Fatalf("GET model = %q ok = %v, want default", model, ok)
	}
	// A JSON body without a model takes the live handler's own default, matching
	// the model the handler will actually route.
	c, _ = directPolicyTestContext(t, http.MethodPost, "/v1/live", []byte(`{"audio":"sdp-not-json"}`))
	if model, ok := server.directRequestedModel(c, "default-model"); !ok || model != codexlive.DefaultLiveModel {
		t.Fatalf("SDP model = %q ok = %v, want handler default %q", model, ok, codexlive.DefaultLiveModel)
	}
	c, _ = directPolicyTestContext(t, http.MethodPost, "/v1/live", nil)
	if model, ok := server.directRequestedModel(c, "default-model"); !ok || model != codexlive.DefaultLiveModel {
		t.Fatalf("empty body model = %q ok = %v, want handler default %q", model, ok, codexlive.DefaultLiveModel)
	}
}

func TestDirectRequestedModelReplaysOversizedBodyAndUsesDefault(t *testing.T) {
	server := &Server{}
	oversize := make([]byte, directModelPolicyBodyLimit+128)
	for i := range oversize {
		oversize[i] = 'a'
	}
	c, _ := directPolicyTestContext(t, http.MethodPost, "/v1/live", oversize)
	model, ok := server.directRequestedModel(c, "default-model")
	if ok || model != "" {
		t.Fatalf("model = %q ok = %v, want rejection on oversize", model, ok)
	}
	if tooLarge, _ := c.Get(directModelPolicyBodyTooLargeKey); tooLarge != true {
		t.Fatal("oversized request was not marked for 413 response")
	}
	rest, _ := io.ReadAll(c.Request.Body)
	if len(rest) != len(oversize) {
		t.Fatalf("replayed body length = %d, want %d", len(rest), len(oversize))
	}
	if !bytes.Equal(rest, oversize) {
		t.Fatal("replayed body content mismatch")
	}
}

func TestApplyDirectPolicyTerminationRelaysPluginResponse(t *testing.T) {
	c, recorder := directPolicyTestContext(t, http.MethodPost, "/v1/live", nil)
	response := pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      http.StatusForbidden,
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		ResponseBody:    []byte(`{"error":"model denied"}`),
	}
	if !applyDirectPolicyTermination(c, response) {
		t.Fatal("termination not reported")
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	body := recorder.Body.String()
	var payload map[string]any
	if errDecode := json.Unmarshal([]byte(body), &payload); errDecode != nil {
		t.Fatalf("body not JSON: %q", body)
	}
	if message, _ := payload["error"].(string); message != "model denied" {
		t.Fatalf("error = %q, want model denied", message)
	}
	if !c.IsAborted() {
		t.Fatal("context not aborted")
	}
}

func TestApplyDirectPolicyTerminationClampsInvalidStatus(t *testing.T) {
	c, recorder := directPolicyTestContext(t, http.MethodPost, "/v1/live", nil)
	if !applyDirectPolicyTermination(c, pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: 200}) {
		t.Fatal("termination not reported")
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want clamped 403", recorder.Code)
	}
}

func TestApplyDirectPolicyTerminationPassesThroughWhenNotTerminating(t *testing.T) {
	c, _ := directPolicyTestContext(t, http.MethodPost, "/v1/live", nil)
	if applyDirectPolicyTermination(c, pluginapi.RequestInterceptResponse{}) {
		t.Fatal("non-terminating response must not terminate")
	}
}

func TestDirectModelPolicyMiddlewareSkipsInspectionWithoutPolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{pluginHost: pluginhost.New()}
	oversize := bytes.Repeat([]byte("x"), directModelPolicyBodyLimit+128)

	router := gin.New()
	router.POST("/v1/live", server.directModelPolicyMiddleware("default-model"), func(c *gin.Context) {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.String(http.StatusInternalServerError, errRead.Error())
			return
		}
		c.String(http.StatusOK, "%d", len(body))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/live", bytes.NewReader(oversize))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got, want := recorder.Body.String(), fmt.Sprintf("%d", len(oversize)); got != want {
		t.Fatalf("downstream body length = %s, want %s", got, want)
	}
}

func TestDirectModelPolicyMiddlewarePassesThroughWithoutPlugins(t *testing.T) {
	server := newTestServerWithOptions(t, WithPluginHost(pluginhost.New()))

	// The empty plugin host has no interceptors, so the middleware must be a
	// no-op and the request must reach the registered live handler instead of
	// being terminated by policy.
	for _, target := range []string{"/v1/live", "/v1/realtime", "/v1/realtime/calls"} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"gpt-live-1-codex"}`))
		req.Header.Set("Authorization", "Bearer test-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "model") {
			t.Fatalf("%s terminated by policy without plugins: status=%d body=%s", target, rr.Code, rr.Body.String())
		}
		if rr.Code == http.StatusBadRequest {
			t.Fatalf("%s rejected as invalid request: body=%s", target, rr.Body.String())
		}
	}
}
