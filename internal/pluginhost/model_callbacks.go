package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// rpcHostModelListRequest selects the protocol view of the model catalog.
// Format mirrors the handler types used by the native model endpoints
// ("openai", "claude", "gemini"). An empty format defaults to "openai".
type rpcHostModelListRequest struct {
	Format string `json:"format,omitempty"`
}

// rpcHostModelListResponse returns the catalog entries verbatim so a plugin
// sees exactly the same model metadata the native endpoints publish.
type rpcHostModelListResponse struct {
	Models []map[string]any `json:"models"`
}

// callHostModelList exposes the routable model catalog to plugins.
//
// Model-governance plugins need the set of models the deployment can actually
// serve in order to present or validate per-key model grants. Reading it
// through a host callback keeps the registry internal while giving plugins the
// same snapshot the native /v1/models endpoints return.
func (h *Host) callHostModelList(ctx context.Context, request []byte) ([]byte, error) {
	_ = ctx
	req := rpcHostModelListRequest{}
	if len(bytesTrimSpace(request)) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host model list request: %w", errUnmarshal)
		}
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "openai"
	}
	var (
		models  []map[string]any
		errList error
	)
	if provider := h.currentModelListProvider(); provider != nil {
		models, errList = provider(ctx, format)
	} else {
		models = registry.GetGlobalRegistry().GetAvailableModels(format)
	}
	if errList != nil {
		return nil, fmt.Errorf("list active host models: %w", errList)
	}
	if models == nil {
		models = make([]map[string]any, 0)
	}
	return marshalRPCResult(rpcHostModelListResponse{Models: models})
}
