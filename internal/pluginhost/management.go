package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/htmlsanitize"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const (
	managementBasePath      = "/v0/management"
	publicControlPlanePath  = "/v0/control-plane"
	resourcePluginBasePath  = "/v0/resource/plugins"
	legacyPluginRoutePrefix = "/plugins"
)

type managementRouteRecord struct {
	pluginID      string
	path          string
	version       string
	schemaVersion uint32
	route         pluginapi.ManagementRoute
}

type resourceRouteRecord struct {
	pluginID string
	path     string
	version  string
	route    pluginapi.ResourceRoute
}

type publicRouteRecord struct {
	pluginID      string
	path          string
	version       string
	schemaVersion uint32
	route         pluginapi.PublicAPIRoute
}

// RegisterManagementRoutes rebuilds the plugin-owned Management API and resource route tables.
func (h *Host) RegisterManagementRoutes(ctx context.Context, reserved map[string]struct{}) {
	if h == nil {
		return
	}

	nextRoutes := make(map[string]managementRouteRecord)
	nextPublic := make(map[string]publicRouteRecord)
	nextResources := make(map[string]resourceRouteRecord)
	for _, record := range h.activeRecords() {
		if h.isPluginFused(record.id) {
			continue
		}
		// The management and public capabilities are independent: a plugin may
		// declare only one of them.
		if plugin := record.plugin.Capabilities.ManagementAPI; plugin != nil {
			resp, errRegister := h.callManagementRegistrar(ctx, record, plugin)
			if errRegister != nil {
				log.Warnf("pluginhost: management registrar %s failed: %v", record.id, errRegister)
			} else {
				h.registerManagementDeclarations(record, resp, reserved, nextRoutes, nextResources)
			}
		}
		if public := record.plugin.Capabilities.PublicAPI; public != nil {
			resp, errRegister := h.callPublicRegistrar(ctx, record, public)
			if errRegister != nil {
				log.Warnf("pluginhost: public API registrar %s failed: %v", record.id, errRegister)
			} else {
				h.registerPublicDeclarations(record, resp, nextPublic)
			}
		}
	}

	h.mu.Lock()
	h.managementRoutes = nextRoutes
	h.publicRoutes = nextPublic
	h.resourceRoutes = nextResources
	h.mu.Unlock()
}

func (h *Host) registerManagementDeclarations(record capabilityRecord, resp pluginapi.ManagementRegistrationResponse, reserved map[string]struct{}, nextRoutes map[string]managementRouteRecord, nextResources map[string]resourceRouteRecord) {
	for _, item := range resp.Routes {
		method, path, okRoute := normalizeManagementRoute(item)
		if !okRoute {
			log.Warnf("pluginhost: plugin %s declared invalid management route %s %s", record.id, item.Method, item.Path)
			continue
		}
		if routeDeclaresLegacyMenuResource(method, item) {
			if !registerResourceRoute(nextResources, record, resourceRouteFromManagementRoute(item)) {
				log.Warnf("pluginhost: plugin %s declared invalid resource route %s", record.id, item.Path)
			}
			continue
		}
		key := managementRouteKey(method, path)
		if _, exists := reserved[key]; exists {
			log.Warnf("pluginhost: plugin %s management route %s conflicts with an existing route and was skipped", record.id, key)
			continue
		}
		if _, exists := nextRoutes[key]; exists {
			log.Warnf("pluginhost: plugin %s management route %s conflicts with a higher-priority plugin and was skipped", record.id, key)
			continue
		}
		item.Method = method
		item.Path = path
		nextRoutes[key] = managementRouteRecord{
			pluginID:      record.id,
			path:          record.path,
			version:       record.version,
			schemaVersion: record.plugin.SchemaVersion,
			route:         item,
		}
	}
	for _, item := range resp.Resources {
		if !registerResourceRoute(nextResources, record, item) {
			log.Warnf("pluginhost: plugin %s declared invalid resource route %s", record.id, item.Path)
		}
	}
}

func (h *Host) callManagementRegistrar(ctx context.Context, record capabilityRecord, plugin pluginapi.ManagementAPI) (resp pluginapi.ManagementRegistrationResponse, err error) {
	if h == nil || plugin == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return pluginapi.ManagementRegistrationResponse{}, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "ManagementAPI.RegisterManagement", recovered)
			resp = pluginapi.ManagementRegistrationResponse{}
			err = fmt.Errorf("management registrar panic: %v", recovered)
		}
	}()
	return plugin.RegisterManagement(ctx, pluginapi.ManagementRegistrationRequest{
		Plugin:           record.meta,
		BasePath:         managementBasePath,
		ResourceBasePath: resourcePluginBasePath + "/" + record.id,
	})
}

func (h *Host) callPublicRegistrar(ctx context.Context, record capabilityRecord, plugin pluginapi.PublicAPI) (resp pluginapi.PublicAPIRegistrationResponse, err error) {
	if h == nil || plugin == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return pluginapi.PublicAPIRegistrationResponse{}, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "PublicAPI.RegisterPublicAPI", recovered)
			resp = pluginapi.PublicAPIRegistrationResponse{}
			err = fmt.Errorf("public API registrar panic: %v", recovered)
		}
	}()
	return plugin.RegisterPublicAPI(ctx, pluginapi.PublicAPIRegistrationRequest{Plugin: record.meta, BasePath: publicControlPlanePath})
}

// registerPublicDeclarations records the plugin-owned public routes declared by
// one plugin, skipping malformed routes and routes already claimed by a
// higher-priority plugin.
func (h *Host) registerPublicDeclarations(record capabilityRecord, resp pluginapi.PublicAPIRegistrationResponse, nextPublic map[string]publicRouteRecord) {
	for _, item := range resp.Routes {
		method, path, okRoute := normalizePublicRoute(item)
		if !okRoute {
			log.Warnf("pluginhost: plugin %s declared invalid public route %s %s", record.id, item.Method, item.Path)
			continue
		}
		key := managementRouteKey(method, path)
		if _, exists := nextPublic[key]; exists {
			log.Warnf("pluginhost: plugin %s public route %s conflicts with a higher-priority plugin and was skipped", record.id, key)
			continue
		}
		item.Method, item.Path = method, path
		nextPublic[key] = publicRouteRecord{
			pluginID:      record.id,
			path:          record.path,
			version:       record.version,
			schemaVersion: record.plugin.SchemaVersion,
			route:         item,
		}
	}
}

func normalizePublicRoute(item pluginapi.PublicAPIRoute) (string, string, bool) {
	if item.Handler == nil {
		return "", "", false
	}
	method := strings.ToUpper(strings.TrimSpace(item.Method))
	if method == "" {
		method = http.MethodGet
	}
	if strings.ContainsAny(method, " \t\r\n") {
		return "", "", false
	}
	path := strings.TrimSpace(item.Path)
	if path == "" {
		return "", "", false
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasPrefix(path, publicControlPlanePath+"/") {
		path = strings.TrimPrefix(path, publicControlPlanePath)
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	fullPath := publicControlPlanePath + path
	if !strings.HasPrefix(fullPath, publicControlPlanePath+"/") || strings.ContainsAny(fullPath, " \t\r\n") || strings.Contains(fullPath, ":") || strings.Contains(fullPath, "*") || strings.Contains(fullPath, "..") {
		return "", "", false
	}
	return method, fullPath, true
}

func normalizeManagementRoute(item pluginapi.ManagementRoute) (string, string, bool) {
	if item.Handler == nil {
		return "", "", false
	}
	method := strings.ToUpper(strings.TrimSpace(item.Method))
	if method == "" {
		method = http.MethodGet
	}
	if strings.ContainsAny(method, " \t\r\n") {
		return "", "", false
	}

	path := strings.TrimSpace(item.Path)
	if path == "" {
		return "", "", false
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasPrefix(path, managementBasePath+"/") {
		path = strings.TrimPrefix(path, managementBasePath)
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "", "", false
	}
	fullPath := managementBasePath + path
	if !strings.HasPrefix(fullPath, managementBasePath+"/") {
		return "", "", false
	}
	if strings.ContainsAny(fullPath, " \t\r\n") || strings.Contains(fullPath, ":") || strings.Contains(fullPath, "*") {
		return "", "", false
	}
	return method, fullPath, true
}

func routeDeclaresLegacyMenuResource(method string, item pluginapi.ManagementRoute) bool {
	return strings.EqualFold(strings.TrimSpace(method), http.MethodGet) && strings.TrimSpace(item.Menu) != ""
}

func resourceRouteFromManagementRoute(item pluginapi.ManagementRoute) pluginapi.ResourceRoute {
	return pluginapi.ResourceRoute{
		Path:        item.Path,
		Menu:        item.Menu,
		Description: item.Description,
		Handler:     item.Handler,
	}
}

func registerResourceRoute(routes map[string]resourceRouteRecord, record capabilityRecord, item pluginapi.ResourceRoute) bool {
	path, okRoute := normalizeResourceRoute(record.id, item)
	if !okRoute {
		return false
	}
	key := managementRouteKey(http.MethodGet, path)
	if _, exists := routes[key]; exists {
		log.Warnf("pluginhost: plugin %s resource route %s conflicts with a higher-priority plugin and was skipped", record.id, key)
		return true
	}
	item.Path = path
	routes[key] = resourceRouteRecord{
		pluginID: record.id,
		path:     record.path,
		version:  record.version,
		route:    item,
	}
	return true
}

func normalizeResourceRoute(pluginID string, item pluginapi.ResourceRoute) (string, bool) {
	if item.Handler == nil {
		return "", false
	}
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return "", false
	}

	path := strings.TrimSpace(item.Path)
	if path == "" {
		return "", false
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	pluginBasePath := resourcePluginBasePath + "/" + pluginID
	if strings.HasPrefix(path, pluginBasePath+"/") {
		path = strings.TrimPrefix(path, pluginBasePath)
	} else if strings.HasPrefix(path, legacyPluginRoutePrefix+"/"+pluginID+"/") {
		path = strings.TrimPrefix(path, legacyPluginRoutePrefix+"/"+pluginID)
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "", false
	}

	fullPath := pluginBasePath + path
	if !strings.HasPrefix(fullPath, pluginBasePath+"/") {
		return "", false
	}
	if strings.ContainsAny(fullPath, " \t\r\n") || strings.Contains(fullPath, ":") || strings.Contains(fullPath, "*") || strings.Contains(fullPath, "..") {
		return "", false
	}
	return fullPath, true
}

func managementRouteKey(method, path string) string {
	return strings.ToUpper(strings.TrimSpace(method)) + " " + strings.TrimSpace(path)
}

// ServeManagementHTTP dispatches an authenticated Management API request to a plugin route.
func (h *Host) ServeManagementHTTP(w http.ResponseWriter, r *http.Request) bool {
	if h == nil || w == nil || r == nil || r.URL == nil {
		return false
	}
	key := managementRouteKey(r.Method, r.URL.Path)
	h.mu.Lock()
	record, okRoute := h.managementRoutes[key]
	h.mu.Unlock()
	if !okRoute || record.route.Handler == nil || h.isPluginFused(record.pluginID) {
		return false
	}

	body, tooLarge, errRead := readPluginRequestBody(r)
	if tooLarge {
		http.Error(w, "plugin management request body is too large", http.StatusRequestEntityTooLarge)
		return true
	}
	if errRead != nil {
		http.Error(w, "failed to read plugin management request body", http.StatusBadRequest)
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	resp, errHandle := h.callManagementHandler(r.Context(), record, pluginapi.ManagementRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: cloneHeader(r.Header),
		Query:   cloneValues(r.URL.Query()),
		Body:    bytes.Clone(body),
	})
	if errHandle != nil {
		log.Warnf("pluginhost: management handler %s failed: %v", record.pluginID, errHandle)
		http.Error(w, "plugin management handler failed", http.StatusBadGateway)
		return true
	}
	if managementResponseEscapesHTML(record.schemaVersion) {
		resp.Body = escapeManagementResponseBody(resp)
	}

	for keyHeader, values := range resp.Headers {
		for _, value := range values {
			w.Header().Add(keyHeader, value)
		}
	}
	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	w.WriteHeader(statusCode)
	if _, errWrite := w.Write(resp.Body); errWrite != nil {
		log.Warnf("pluginhost: failed to write plugin management response: %v", errWrite)
	}
	return true
}

// PublicRouteServed reports whether a plugin currently serves a public route
// for the method and path, either as an exact route or through prefix
// dispatch below /v0/control-plane/, e.g. GET /v0/control-plane/ui.js for
// control-plane UI injection.
func (h *Host) PublicRouteServed(method, path string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.publicRoutes[managementRouteKey(method, path)]; ok {
		return true
	}
	prefix := publicControlPlanePath + "/"
	if strings.HasPrefix(path, prefix) {
		_, ok := h.publicRoutes[managementRouteKey(method, prefix)]
		return ok
	}
	return false
}

// maxPluginRequestBodyBytes bounds a plugin-dispatched request body.
//
// Plugin management and public payloads are administrative JSON, so this is far
// above any legitimate request while still preventing a single caller from
// buffering unbounded data in host memory.
const maxPluginRequestBodyBytes int64 = 8 << 20

// readPluginRequestBody reads a bounded request body.
//
// It reports tooLarge separately from a read error so the caller can answer 413
// rather than 400: an oversize body is a rejected request, not a broken one.
func readPluginRequestBody(r *http.Request) (body []byte, tooLarge bool, err error) {
	if r == nil || r.Body == nil {
		return nil, false, nil
	}
	defer func() {
		if errClose := r.Body.Close(); errClose != nil {
			log.Warnf("pluginhost: failed to close plugin request body: %v", errClose)
		}
	}()
	// Read one byte past the limit so an exactly-at-limit body still succeeds
	// while anything larger is detected without buffering it all.
	limited := io.LimitReader(r.Body, maxPluginRequestBodyBytes+1)
	buffered, errRead := io.ReadAll(limited)
	if errRead != nil {
		return nil, false, errRead
	}
	if int64(len(buffered)) > maxPluginRequestBodyBytes {
		return nil, true, nil
	}
	return buffered, false, nil
}

// ServePublicAPIHTTP dispatches a plugin-owned control-plane route. Unlike
// management routes, this path intentionally does not apply the host's
// operator-password middleware; the plugin owns session and RBAC checks.
func (h *Host) ServePublicAPIHTTP(w http.ResponseWriter, r *http.Request) bool {
	if h == nil || w == nil || r == nil || r.URL == nil {
		return false
	}
	key := managementRouteKey(r.Method, r.URL.Path)
	h.mu.Lock()
	record, okRoute := h.publicRoutes[key]
	if !okRoute && strings.HasPrefix(r.URL.Path, publicControlPlanePath+"/") {
		record, okRoute = h.publicRoutes[managementRouteKey(r.Method, publicControlPlanePath+"/")]
	}
	h.mu.Unlock()
	if !okRoute || record.route.Handler == nil || h.isPluginFused(record.pluginID) {
		return false
	}
	// Public routes are unauthenticated by design: the plugin owns the session
	// check, which cannot run until the request has been forwarded. The body is
	// therefore bounded here so an anonymous caller cannot exhaust host memory
	// before any credential is evaluated.
	body, tooLarge, errRead := readPluginRequestBody(r)
	if tooLarge {
		http.Error(w, "plugin public request body is too large", http.StatusRequestEntityTooLarge)
		return true
	}
	if errRead != nil {
		http.Error(w, "failed to read plugin public request body", http.StatusBadRequest)
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	resp, errHandle := h.callPublicHandler(r.Context(), record, pluginapi.ManagementRequest{
		Method: r.Method, Path: r.URL.Path, Headers: cloneHeader(r.Header), Query: cloneValues(r.URL.Query()), Body: bytes.Clone(body),
	})
	if errHandle != nil {
		log.Warnf("pluginhost: public handler %s failed: %v", record.pluginID, errHandle)
		http.Error(w, "plugin public handler failed", http.StatusBadGateway)
		return true
	}
	for keyHeader, values := range resp.Headers {
		for _, value := range values {
			w.Header().Add(keyHeader, value)
		}
	}
	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	w.WriteHeader(statusCode)
	if _, errWrite := w.Write(resp.Body); errWrite != nil {
		log.Warnf("pluginhost: failed to write plugin public response: %v", errWrite)
	}
	return true
}

// ServeResourceHTTP dispatches an unauthenticated browser-navigable resource request to a plugin route.
func (h *Host) ServeResourceHTTP(w http.ResponseWriter, r *http.Request) bool {
	if h == nil || w == nil || r == nil || r.URL == nil {
		return false
	}
	if !strings.EqualFold(r.Method, http.MethodGet) {
		return false
	}
	key := managementRouteKey(http.MethodGet, r.URL.Path)
	h.mu.Lock()
	record, okRoute := h.resourceRoutes[key]
	h.mu.Unlock()
	if !okRoute || record.route.Handler == nil || h.isPluginFused(record.pluginID) {
		return false
	}

	resp, errHandle := h.callResourceHandler(r.Context(), record, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    r.URL.Path,
		Headers: cloneHeader(r.Header),
		Query:   cloneValues(r.URL.Query()),
	})
	if errHandle != nil {
		log.Warnf("pluginhost: resource handler %s failed: %v", record.pluginID, errHandle)
		http.Error(w, "plugin resource handler failed", http.StatusBadGateway)
		return true
	}

	for keyHeader, values := range resp.Headers {
		for _, value := range values {
			w.Header().Add(keyHeader, value)
		}
	}
	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	w.WriteHeader(statusCode)
	if _, errWrite := w.Write(resp.Body); errWrite != nil {
		log.Warnf("pluginhost: failed to write plugin resource response: %v", errWrite)
	}
	return true
}

func (h *Host) callManagementHandler(ctx context.Context, record managementRouteRecord, req pluginapi.ManagementRequest) (resp pluginapi.ManagementResponse, err error) {
	if h == nil || record.route.Handler == nil || h.isPluginFused(record.pluginID) || !h.pluginIdentityCurrent(record.pluginID, record.path, record.version) {
		return pluginapi.ManagementResponse{}, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.pluginID, "ManagementHandler.HandleManagement", recovered)
			resp = pluginapi.ManagementResponse{}
			err = fmt.Errorf("management handler panic: %v", recovered)
		}
	}()
	return record.route.Handler.HandleManagement(ctx, req)
}

func (h *Host) callPublicHandler(ctx context.Context, record publicRouteRecord, req pluginapi.ManagementRequest) (resp pluginapi.ManagementResponse, err error) {
	if h == nil || record.route.Handler == nil || h.isPluginFused(record.pluginID) || !h.pluginIdentityCurrent(record.pluginID, record.path, record.version) {
		return pluginapi.ManagementResponse{}, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.pluginID, "PublicAPI.HandleManagement", recovered)
			resp = pluginapi.ManagementResponse{}
			err = fmt.Errorf("public handler panic: %v", recovered)
		}
	}()
	return record.route.Handler.HandleManagement(ctx, req)
}

func escapeManagementResponseBody(resp pluginapi.ManagementResponse) []byte {
	body, okEscaped := htmlsanitize.JSONBodyIfLikely(resp.Body, resp.Headers.Get("Content-Type"))
	if !okEscaped {
		return resp.Body
	}
	return body
}

func managementResponseEscapesHTML(schemaVersion uint32) bool {
	return schemaVersion < pluginabi.SchemaVersionRawManagementResponse
}

func (h *Host) callResourceHandler(ctx context.Context, record resourceRouteRecord, req pluginapi.ManagementRequest) (resp pluginapi.ManagementResponse, err error) {
	if h == nil || record.route.Handler == nil || h.isPluginFused(record.pluginID) || !h.pluginIdentityCurrent(record.pluginID, record.path, record.version) {
		return pluginapi.ManagementResponse{}, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.pluginID, "ResourceHandler.HandleManagement", recovered)
			resp = pluginapi.ManagementResponse{}
			err = fmt.Errorf("resource handler panic: %v", recovered)
		}
	}()
	return record.route.Handler.HandleManagement(ctx, req)
}
