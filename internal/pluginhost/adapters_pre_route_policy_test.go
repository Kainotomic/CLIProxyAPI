package pluginhost

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type preRoutePolicyFunc func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error)

func (f preRoutePolicyFunc) CheckPreRoutePolicy(ctx context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	return f(ctx, req)
}

func TestHasActivePreRoutePolicyIncludesFusedPolicy(t *testing.T) {
	if (*Host)(nil).HasActivePreRoutePolicy("") {
		t.Fatal("nil host reported an active pre-route policy")
	}
	if New().HasActivePreRoutePolicy("") {
		t.Fatal("empty host reported an active pre-route policy")
	}

	const pluginID = "governance"
	policy := preRoutePolicyFunc(func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
		return pluginapi.RequestInterceptResponse{}, nil
	})
	host := newHostWithRecords(capabilityRecord{
		id: pluginID,
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			PreRoutePolicy: policy,
		}},
	})
	if !host.HasActivePreRoutePolicy("") {
		t.Fatal("declared pre-route policy was not detected")
	}
	if host.HasActivePreRoutePolicy(pluginID) {
		t.Fatal("skipped policy was reported as active")
	}

	host.fusePlugin(pluginID, "test", "sentinel panic")
	if !host.HasActivePreRoutePolicy("") {
		t.Fatal("fused declared policy was not retained as an admission requirement")
	}
}

func TestCheckPreRoutePolicyFailsClosedAfterPolicyFuse(t *testing.T) {
	const pluginID = "governance"
	calls := 0
	policy := preRoutePolicyFunc(func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
		calls++
		panic("policy panic")
	})
	host := newHostWithRecords(capabilityRecord{
		id: pluginID,
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			PreRoutePolicy: policy,
		}},
	})

	first := host.CheckPreRoutePolicy(context.Background(), pluginapi.RequestInterceptRequest{Model: "denied-model"})
	assertPreRouteUnavailable(t, first)
	if calls != 1 {
		t.Fatalf("policy calls after first check = %d, want 1", calls)
	}

	// The panic opens the plugin-wide fuse. Every later request must continue
	// failing closed instead of skipping the now-fused policy.
	second := host.CheckPreRoutePolicy(context.Background(), pluginapi.RequestInterceptRequest{Model: "denied-model"})
	assertPreRouteUnavailable(t, second)
	if calls != 1 {
		t.Fatalf("fused policy was called again: calls = %d, want 1", calls)
	}

	// Nested work owned by this plugin may explicitly skip its own policy. The
	// skip remains the only exception to fail-closed fused-policy handling.
	skipped := host.CheckPreRoutePolicyExcept(context.Background(), pluginapi.RequestInterceptRequest{Model: "denied-model"}, pluginID)
	if skipped.Terminate {
		t.Fatalf("explicitly skipped policy terminated request: %+v", skipped)
	}
}

func TestCheckPreRoutePolicyFailsClosedWhenAnotherCapabilityFusesPlugin(t *testing.T) {
	const pluginID = "governance"
	calls := 0
	policy := preRoutePolicyFunc(func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
		calls++
		return pluginapi.RequestInterceptResponse{}, nil
	})
	host := newHostWithRecords(capabilityRecord{
		id: pluginID,
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			PreRoutePolicy: policy,
		}},
	})
	host.fusePlugin(pluginID, "UsagePlugin.HandleUsage", "unrelated capability panic")

	response := host.CheckPreRoutePolicy(context.Background(), pluginapi.RequestInterceptRequest{Model: "denied-model"})
	assertPreRouteUnavailable(t, response)
	if calls != 0 {
		t.Fatalf("fused policy calls = %d, want 0", calls)
	}
}

func assertPreRouteUnavailable(t *testing.T, response pluginapi.RequestInterceptResponse) {
	t.Helper()
	if !response.Terminate {
		t.Fatal("unavailable pre-route policy did not terminate request")
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if got := response.ResponseHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if string(response.ResponseBody) != `{"error":"pre-route policy unavailable"}` {
		t.Fatalf("body = %q, want unavailable error", response.ResponseBody)
	}
}
