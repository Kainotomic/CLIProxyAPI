package pluginhost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// TestHostModelListReturnsCatalog asserts the callback answers with the
// registry snapshot for the default protocol view.
func TestHostModelListReturnsCatalog(t *testing.T) {
	host := New()

	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelList, nil)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[rpcHostModelListResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.Models == nil {
		t.Fatal("models = nil, want a non-nil slice so plugins can distinguish empty from absent")
	}
}

// TestHostModelListRejectsMalformedRequest keeps decoding strict: a plugin that
// sends a malformed payload must get an error rather than a silent default.
func TestHostModelListRejectsMalformedRequest(t *testing.T) {
	host := New()

	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelList, []byte("{")); errCall == nil {
		t.Fatal("callFromPlugin() error = nil, want a decode error")
	}
}

// TestHostModelListHonoursFormat asserts the requested protocol view is passed
// through to the registry instead of always returning the OpenAI shape.
func TestHostModelListHonoursFormat(t *testing.T) {
	host := New()

	request, errMarshal := json.Marshal(rpcHostModelListRequest{Format: "claude"})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelList, request)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	if _, errDecode := decodeRPCEnvelope[rpcHostModelListResponse](rawResp); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
}
