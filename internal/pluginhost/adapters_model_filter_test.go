package pluginhost

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type modelFilterFunc func(context.Context, pluginapi.ModelFilterRequest) (pluginapi.ModelFilterResponse, error)

func (f modelFilterFunc) FilterModels(ctx context.Context, req pluginapi.ModelFilterRequest) (pluginapi.ModelFilterResponse, error) {
	return f(ctx, req)
}

func TestFilterModelsFailsClosedAfterFilterFuse(t *testing.T) {
	const pluginID = "model-governance"
	calls := 0
	filter := modelFilterFunc(func(context.Context, pluginapi.ModelFilterRequest) (pluginapi.ModelFilterResponse, error) {
		calls++
		panic("filter panic")
	})
	host := newHostWithRecords(capabilityRecord{
		id: pluginID,
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			ModelFilter: filter,
		}},
	})
	native := []map[string]any{{"id": "private-model"}}

	models, status, message := host.FilterModels(context.Background(), "/v1/models", http.Header{}, url.Values{}, nil, native)
	assertModelFilterUnavailable(t, models, status, message)
	if calls != 1 {
		t.Fatalf("filter calls after first request = %d, want 1", calls)
	}

	// The panic opened the plugin-wide fuse. Later requests must keep failing
	// closed instead of receiving the unfiltered native catalog.
	models, status, message = host.FilterModels(context.Background(), "/v1/models", http.Header{}, url.Values{}, nil, native)
	assertModelFilterUnavailable(t, models, status, message)
	if calls != 1 {
		t.Fatalf("fused filter was called again: calls = %d, want 1", calls)
	}
}

func TestFilterModelsFailsClosedWhenAnotherCapabilityFusesPlugin(t *testing.T) {
	const pluginID = "model-governance"
	calls := 0
	filter := modelFilterFunc(func(context.Context, pluginapi.ModelFilterRequest) (pluginapi.ModelFilterResponse, error) {
		calls++
		return pluginapi.ModelFilterResponse{Handled: true, Models: []map[string]any{}}, nil
	})
	host := newHostWithRecords(capabilityRecord{
		id: pluginID,
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			ModelFilter: filter,
		}},
	})
	host.fusePlugin(pluginID, "UsagePlugin.HandleUsage", "unrelated capability panic")

	models, status, message := host.FilterModels(context.Background(), "/v1/models", http.Header{}, url.Values{}, nil, []map[string]any{{"id": "private-model"}})
	assertModelFilterUnavailable(t, models, status, message)
	if calls != 0 {
		t.Fatalf("fused filter calls = %d, want 0", calls)
	}
}

func TestFilterModelsStillSkipsPluginsWithoutFilter(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{
		id:     "unrelated",
		plugin: pluginapi.Plugin{},
	})
	native := []map[string]any{{"id": "public-model"}}

	models, status, message := host.FilterModels(context.Background(), "/v1/models", http.Header{}, url.Values{}, nil, native)
	if status != 0 || message != "" {
		t.Fatalf("status = %d message = %q, want success", status, message)
	}
	if len(models) != 1 || models[0]["id"] != "public-model" {
		t.Fatalf("models = %#v, want native catalog", models)
	}
}

func assertModelFilterUnavailable(t *testing.T, models []map[string]any, status int, message string) {
	t.Helper()
	if models != nil {
		t.Fatalf("models = %#v, want nil on unavailable filter", models)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if message != "model filter unavailable" {
		t.Fatalf("message = %q, want model filter unavailable", message)
	}
}
