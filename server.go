package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg      Config
	usage    *UsageService
	basePath string
	build    BuildMetadata
	handler  http.Handler
}

func NewServer(cfg Config, usage *UsageService) *Server {
	server := &Server{cfg: cfg, usage: usage, basePath: cfg.BasePath, build: currentBuildMetadata()}
	mux := http.NewServeMux()
	server.registerRoutes(mux, cfg.BasePath)
	if cfg.BasePath != "" {
		// Also accept root routes for reverse proxies that strip /codex before
		// forwarding the request to this Go process.
		server.registerRoutes(mux, "")
		mux.HandleFunc("GET "+cfg.BasePath, server.handleBaseRedirect)
	}
	server.handler = server.withMiddleware(mux)
	return server
}

func (s *Server) registerRoutes(mux *http.ServeMux, prefix string) {
	route := func(suffix string) string {
		if prefix == "" {
			return suffix
		}
		if suffix == "/" {
			return prefix + "/"
		}
		return prefix + suffix
	}
	mux.HandleFunc("GET "+route("/healthz"), s.handleHealth)
	mux.HandleFunc("GET "+route("/"), s.handleIndex)
	mux.HandleFunc("GET "+route("/settings"), s.handleSettings)
	mux.HandleFunc("GET "+route("/api-docs"), s.handleAPIDocs)
	mux.HandleFunc("GET "+route("/api-docs/"), s.handleAPIDocs)
	mux.HandleFunc("GET "+route("/openapi.yaml"), s.handleOpenAPISpec)
	mux.HandleFunc("GET "+route("/assets/account-credentials-guide.png"), s.handleCredentialGuide)
	mux.HandleFunc("GET "+route("/audio"), s.handleAudio)
	mux.HandleFunc("GET "+route("/api/usage"), s.handleUsage)
	mux.HandleFunc("GET "+route("/api/usage/analytics"), s.handleUsageAnalytics)
	mux.HandleFunc("GET "+route("/api/prediction"), s.handlePrediction)
	mux.HandleFunc("GET "+route("/api/config"), s.handleConfigGet)
	mux.HandleFunc("GET "+route("/api/config/app-key"), s.handleConfigAppKey)
	mux.HandleFunc("GET "+route("/api/config/file"), s.handleConfigFileGet)
	mux.HandleFunc("POST "+route("/api/config/test"), s.handleConfigTest)
	mux.HandleFunc("POST "+route("/api/config/test-proxy"), s.handleProxyTest)
	mux.HandleFunc("PUT "+route("/api/config"), s.handleConfigPut)
	mux.HandleFunc("PUT "+route("/api/config/file"), s.handleConfigFilePut)
	mux.HandleFunc("OPTIONS "+route("/api/usage"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/usage/analytics"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/prediction"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/app-key"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/file"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/test"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/test-proxy"), s.handleOptions)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		build := s.buildMetadata()
		response.Header().Set("X-Codex-Meter-Version", build.Version)
		response.Header().Set("X-Codex-Meter-Commit", build.Commit)
		response.Header().Set("X-Codex-Meter-Build-Time", build.BuildTime)
		middlewareConfig := s.cfg
		if s.usage != nil {
			middlewareConfig = s.usage.currentConfig()
		}
		if middlewareConfig.CORSOrigin != "" {
			response.Header().Set("Access-Control-Allow-Origin", middlewareConfig.CORSOrigin)
			response.Header().Set("Vary", "Origin")
			response.Header().Set("Access-Control-Allow-Headers", "Authorization, X-App-API-Key, Content-Type")
			response.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
			response.Header().Set("Access-Control-Expose-Headers", "X-Codex-Meter-Version, X-Codex-Meter-Commit, X-Codex-Meter-Build-Time")
		}
		if s.isHealthPath(request.URL.Path) {
			next.ServeHTTP(response, request)
			return
		}
		if middlewareConfig.BasicAuthEnabled && !authorizedBasic(request, middlewareConfig.BasicAuthUsername, middlewareConfig.BasicAuthPassword) {
			response.Header().Set("WWW-Authenticate", `Basic realm="Codex Usage"`)
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s.isConfigFilePath(request.URL.Path) && !middlewareConfig.BasicAuthEnabled && middlewareConfig.AppAPIKey == "" {
			writeJSON(response, http.StatusForbidden, map[string]string{"error": "config file access requires Basic Auth or App API Key"})
			return
		}
		// Basic Auth is the page/API authentication mechanism. If it is enabled
		// and already passed, do not require a second app API key as well. The
		// app key remains available for deployments that do not use Basic Auth.
		appKeyRequired := s.isAPIPath(request.URL.Path) && middlewareConfig.AppAPIKey != ""
		basicAuthPassed := middlewareConfig.BasicAuthEnabled && authorizedBasic(request, middlewareConfig.BasicAuthUsername, middlewareConfig.BasicAuthPassword)
		if appKeyRequired && !basicAuthPassed && !authorized(request, middlewareConfig.AppAPIKey) {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) isHealthPath(requestPath string) bool {
	if requestPath == "/healthz" {
		return true
	}
	return s.basePath != "" && requestPath == s.basePath+"/healthz"
}

func (s *Server) isAPIPath(requestPath string) bool {
	if strings.HasPrefix(requestPath, "/api/") {
		return true
	}
	return s.basePath != "" && strings.HasPrefix(requestPath, s.basePath+"/api/")
}

func (s *Server) isConfigFilePath(requestPath string) bool {
	if requestPath == "/api/config/file" {
		return true
	}
	return s.basePath != "" && requestPath == s.basePath+"/api/config/file"
}

func authorized(request *http.Request, expected string) bool {
	if provided := strings.TrimSpace(request.Header.Get("X-App-API-Key")); provided != "" {
		return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
	}
	const prefix = "Bearer "
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func authorizedBasic(request *http.Request, expectedUser, expectedPassword string) bool {
	providedUser, providedPassword, ok := request.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(providedUser), []byte(expectedUser)) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(providedPassword), []byte(expectedPassword)) == 1
	return userOK && passwordOK
}

func (s *Server) handleHealth(response http.ResponseWriter, _ *http.Request) {
	build := s.buildMetadata()
	writeJSON(response, http.StatusOK, HealthResponse{
		Status:    "ok",
		Version:   build.Version,
		Commit:    build.Commit,
		BuildTime: build.BuildTime,
	})
}

func (s *Server) handleIndex(response http.ResponseWriter, request *http.Request) {
	config := s.cfg
	if s.usage != nil {
		config = s.usage.currentConfig()
	}
	if config.SetupRequired {
		s.writeHTML(response, setupHTML)
		return
	}
	page := indexHTML
	if !isDeviceWebViewRequest(request) {
		page = browserHTML
	}
	s.writeHTML(response, page)
}

func isDeviceWebViewRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	userAgent := strings.ToLower(request.Header.Get("User-Agent"))
	if strings.Contains(userAgent, "lx04") {
		return true
	}
	if !strings.Contains(userAgent, "android") {
		return false
	}
	return strings.Contains(userAgent, "; wv") || strings.Contains(userAgent, "version/4.0") || strings.Contains(userAgent, "androidstream")
}

func (s *Server) handleSettings(response http.ResponseWriter, _ *http.Request) {
	s.writeHTML(response, settingsHTML)
}

func (s *Server) handleAPIDocs(response http.ResponseWriter, _ *http.Request) {
	s.writeHTML(response, apiDocsHTML)
}

func (s *Server) buildMetadata() BuildMetadata {
	if s != nil && s.build.Version != "" {
		return s.build
	}
	return currentBuildMetadata()
}

func (s *Server) writeHTML(response http.ResponseWriter, page []byte) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(renderHTMLBuildMetadata(page, s.buildMetadata()))
}

func (s *Server) handleOpenAPISpec(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	response.Header().Set("Content-Disposition", `inline; filename="openapi.yaml"`)
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(openAPISpec)
}

func (s *Server) handleCredentialGuide(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "image/png")
	response.Header().Set("Cache-Control", "private, max-age=3600")
	response.Header().Set("Content-Length", strconv.Itoa(len(accountCredentialsGuide)))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(accountCredentialsGuide)
}

func (s *Server) handleAudio(response http.ResponseWriter, request *http.Request) {
	kind := strings.TrimSpace(request.URL.Query().Get("kind"))
	data, err := embeddedAudio(kind)
	if err != nil {
		writeJSON(response, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	response.Header().Set("Content-Type", "audio/wav")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	response.Header().Set("Content-Length", strconv.Itoa(len(data)))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(data)
}

func embeddedAudio(kind string) ([]byte, error) {
	markers := map[string]string{
		"normal":   "quotaAlertAudio = new Audio('data:audio/wav;base64,",
		"warning":  "quotaAlertAudioWarning = new Audio('data:audio/wav;base64,",
		"critical": "quotaAlertAudioCritical = new Audio('data:audio/wav;base64,",
	}
	marker, ok := markers[kind]
	if !ok {
		return nil, fmt.Errorf("unknown audio kind %q", kind)
	}
	start := bytes.Index(indexHTML, []byte(marker))
	if start < 0 {
		return nil, fmt.Errorf("embedded audio %q was not found", kind)
	}
	start += len(marker)
	endOffset := bytes.Index(indexHTML[start:], []byte("');"))
	if endOffset < 0 {
		return nil, fmt.Errorf("embedded audio %q is malformed", kind)
	}
	encoded := bytes.TrimSpace(indexHTML[start : start+endOffset])
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode embedded audio %q: %w", kind, err)
	}
	return decoded, nil
}

func (s *Server) handleBaseRedirect(response http.ResponseWriter, request *http.Request) {
	location := request.URL.Path + "/"
	if request.URL.RawQuery != "" {
		location += "?" + request.URL.RawQuery
	}
	http.Redirect(response, request, location, http.StatusPermanentRedirect)
}

func (s *Server) handleOptions(response http.ResponseWriter, _ *http.Request) {
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUsage(response http.ResponseWriter, request *http.Request) {
	force, err := parseBool(request.URL.Query().Get("force"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "force must be true or false"})
		return
	}

	usage, err := s.usage.Get(request.Context(), force)
	if err != nil {
		slog.Error("usage request failed", "error", err)
		writeJSON(response, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, usage)
}

func (s *Server) handleUsageAnalytics(response http.ResponseWriter, request *http.Request) {
	force, err := parseBool(request.URL.Query().Get("force"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "force must be true or false"})
		return
	}

	dateRange, err := parseAnalyticsDateRange(request.URL.Query(), time.Now())
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	analytics, err := s.usage.GetAnalyticsRange(request.Context(), force, dateRange)
	if err != nil {
		slog.Error("usage analytics fetch failed", "error", err)
		writeJSON(response, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, analytics)
}

func (s *Server) handlePrediction(response http.ResponseWriter, request *http.Request) {
	force, err := parseBool(request.URL.Query().Get("force"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "force must be true or false"})
		return
	}

	prediction, err := s.usage.GetPrediction(request.Context(), force)
	if err != nil {
		slog.Error("reset prediction fetch failed", "error", err)
		writeJSON(response, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, prediction)
}

func (s *Server) handleConfigGet(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.usage.ConfigView())
}

func (s *Server) handleConfigAppKey(response http.ResponseWriter, _ *http.Request) {
	// This endpoint is intentionally protected by the same middleware as the
	// other management APIs. It only exposes the explicitly requested App API
	// Key and never logs or caches the value.
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	cfg := s.usage.currentConfig()
	writeJSON(response, http.StatusOK, map[string]any{
		"configured":  cfg.AppAPIKey != "",
		"app_api_key": cfg.AppAPIKey,
	})
}

func (s *Server) handleConfigFileGet(response http.ResponseWriter, _ *http.Request) {
	view, err := s.usage.ReadConfigFile()
	if err != nil {
		writeJSON(response, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) handleConfigFilePut(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, maxConfigFileSize+4096)
	var input ConfigFileUpdate
	if err := json.NewDecoder(io.LimitReader(request.Body, maxConfigFileSize+4096)).Decode(&input); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid config file request"})
		return
	}
	view, err := s.usage.UpdateConfigFile(input.Content)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) handleConfigTest(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var update ConfigUpdate
	if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&update); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	result, err := s.usage.TestConfig(request.Context(), update)
	if err != nil {
		// The error intentionally contains only the upstream status or a safe
		// network/validation message; credentials are never included.
		writeJSON(response, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleProxyTest(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input ProxyTestRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 8<<10)).Decode(&input); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	result, err := s.usage.TestProxy(request.Context(), input.ProxyURL)
	if err != nil {
		writeJSON(response, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleConfigPut(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var update ConfigUpdate
	if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&update); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	view, err := s.usage.UpdateConfig(update)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func parseBool(raw string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
