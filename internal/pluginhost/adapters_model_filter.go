package pluginhost

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func pluginPanicError(operation string, recovered any) error {
	return fmt.Errorf("%s panic: %v", operation, recovered)
}

// FilterModels lets the active plugin chain apply access-specific visibility to
// a native model list. Plugins that do not own the request return Handled=false.
func (h *Host) FilterModels(ctx context.Context, path string, headers http.Header, query url.Values, accessMetadata map[string]string, models []map[string]any) ([]map[string]any, int, string) {
	if h == nil {
		return models, 0, ""
	}
	current := models
	for _, record := range h.activeRecords() {
		filter := record.plugin.Capabilities.ModelFilter
		if filter == nil {
			continue
		}
		// A declared model filter remains an access-control requirement after
		// the plugin fuse opens. Skipping it would expose the unfiltered native
		// catalog to every later caller until the plugin reloads.
		if h.isPluginFused(record.id) {
			return nil, http.StatusServiceUnavailable, "model filter unavailable"
		}
		if !h.recordCurrent(record) {
			return nil, http.StatusServiceUnavailable, "model filter unavailable"
		}
		resp, errFilter := func() (resp pluginapi.ModelFilterResponse, err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					h.fusePlugin(record.id, "ModelFilter.FilterModels", recovered)
					err = pluginPanicError("ModelFilter.FilterModels", recovered)
				}
			}()
			return filter.FilterModels(ctx, pluginapi.ModelFilterRequest{Path: path, Headers: headers, Query: query, Models: current, AccessMetadata: accessMetadata})
		}()
		if errFilter != nil {
			log.Warnf("pluginhost: model filter %s failed: %v", record.id, errFilter)
			return nil, http.StatusServiceUnavailable, "model filter unavailable"
		}
		if !resp.Handled {
			continue
		}
		if resp.StatusCode != 0 && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			return nil, resp.StatusCode, resp.Error
		}
		current = resp.Models
		if current == nil {
			current = make([]map[string]any, 0)
		}
	}
	return current, 0, ""
}
