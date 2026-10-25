package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type server struct {
	store         *Store
	upstream      *upstream
	reqTimeout    time.Duration
	streamTimeout time.Duration
	sessionTTL    time.Duration
}

func (s *server) routes(mux *http.ServeMux) {
	// Public OpenAI-compatible API.
	mux.HandleFunc("POST /v1/chat/completions", s.handleCompletions)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /llms.txt", s.handleLLMsTxt)
	mux.HandleFunc("OPTIONS /", s.handleCORS)

	// Admin session.
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.requireAdmin(s.handleMe))

	// Admin: providers.
	mux.HandleFunc("GET /api/providers", s.requireAdmin(s.handleListProviders))
	mux.HandleFunc("POST /api/providers", s.requireAdmin(s.handleAddProvider))
	mux.HandleFunc("POST /api/providers/reorder", s.requireAdmin(s.handleReorderProviders))
	mux.HandleFunc("PUT /api/providers/{id}", s.requireAdmin(s.handleUpdateProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", s.requireAdmin(s.handleDeleteProvider))
	mux.HandleFunc("POST /api/providers/{id}/test", s.requireAdmin(s.handleTestProvider))

	// Admin: API keys.
	mux.HandleFunc("GET /api/keys", s.requireAdmin(s.handleListKeys))
	mux.HandleFunc("POST /api/keys", s.requireAdmin(s.handleCreateKey))
	mux.HandleFunc("DELETE /api/keys/{id}", s.requireAdmin(s.handleRevokeKey))

	// Admin: usage.
	mux.HandleFunc("GET /api/usage", s.requireAdmin(s.handleUsage))

	// Static admin site.
	mux.HandleFunc("/", s.handleStatic)
}

func (s *server) startSessionCleanup() {
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			s.store.cleanupSessions()
		}
	}()
}

// ---------- Completions (public) ----------

func (s *server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	setCORS(w)

	key := s.apiKeyFromRequest(r)
	if !s.store.validateAPIKey(key) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}
	s.store.recordAPIKeyUse(key)

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	req, err := parseIncoming(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}

	providers := s.store.enabledProviders()
	if len(providers) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no enabled providers configured")
		return
	}

	if req.Stream {
		s.streamCompletions(w, r, req, providers)
		return
	}
	s.plainCompletions(w, r, req, providers)
}

func (s *server) plainCompletions(w http.ResponseWriter, r *http.Request, req *incomingRequest, providers []Provider) {
	ctx, cancel := context.WithTimeout(r.Context(), s.reqTimeout)
	defer cancel()

	var attempts []string
	for _, p := range providers {
		start := time.Now()
		res, err := s.upstream.completion(ctx, p, req)
		lat := time.Since(start).Milliseconds()
		model := resolveModel(p, req)
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %s", p.Name, err.Error()))
			s.store.recordUsage(p.ID, p.Name, model, "error", 0, 0, lat, err.Error())
			log.Printf("[completion] provider %q failed: %v", p.Name, err)
			continue
		}
		s.store.recordUsage(p.ID, p.Name, model, "success", res.promptTokens, res.completionTokens, lat, "")
		log.Printf("[completion] served by %q (%dms, %d tokens)", p.Name, lat, res.completionTokens)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if res.rawBody != nil {
			_, _ = w.Write(res.rawBody)
		} else {
			_ = json.NewEncoder(w).Encode(res.body)
		}
		return
	}

	msg := "all providers failed"
	if len(attempts) > 0 {
		msg = "all providers failed: " + strings.Join(attempts, "; ")
	}
	log.Printf("[completion] %s", msg)
	writeOpenAIError(w, http.StatusBadGateway, msg)
}

func (s *server) streamCompletions(w http.ResponseWriter, r *http.Request, req *incomingRequest, providers []Provider) {
	var lastErr string
	for _, p := range providers {
		start := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), s.streamTimeout)
		usage, err := s.upstream.streamCompletion(ctx, p, req, w, httpFlusher(w))
		cancel()
		lat := time.Since(start).Milliseconds()
		model := resolveModel(p, req)
		if err != nil {
			lastErr = err.Error()
			s.store.recordUsage(p.ID, p.Name, model, "error", 0, 0, lat, err.Error())
			log.Printf("[completion] stream provider %q failed to start: %v", p.Name, err)
			continue
		}
		pt, ct := int64(0), int64(0)
		if usage != nil {
			pt, ct = usage.promptTokens, usage.completionTokens
		}
		s.store.recordUsage(p.ID, p.Name, model, "success", pt, ct, lat, "")
		log.Printf("[completion] stream served by %q", p.Name)
		return
	}

	msg := "all providers failed"
	if lastErr != "" {
		msg = "all providers failed: " + lastErr
	}
	writeOpenAIError(w, http.StatusBadGateway, msg)
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	key := s.apiKeyFromRequest(r)
	if !s.store.validateAPIKey(key) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}
	providers := s.store.enabledProviders()
	data := []map[string]any{}
	for _, p := range providers {
		model := p.Model
		if model == "" {
			model = "*"
		}
		data = append(data, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": p.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/llms.txt")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (s *server) handleCORS(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Admin: session ----------

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if !s.store.checkPassword(req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong password"})
		return
	}
	tok := s.store.createSession(s.sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"token": tok})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		s.store.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- Admin: providers ----------

func (s *server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.store.listProviders()})
}

func (s *server) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	var p Provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if err := validateProvider(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	p = s.store.addProvider(p)
	writeJSON(w, http.StatusCreated, p)
}

func (s *server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p Provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if err := validateProvider(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	updated, ok := s.store.updateProvider(id, p)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found"})
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.store.deleteProvider(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleReorderProviders(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if !s.store.reorderProviders(req.IDs) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ids must include every provider exactly once"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.store.listProviders()})
}

func (s *server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	providers := s.store.listProviders()
	var target *Provider
	for i := range providers {
		if providers[i].ID == id {
			target = &providers[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found"})
		return
	}

	probe := "Say hi in one short sentence."
	req := &incomingRequest{
		Model:    target.Model,
		Messages: []incomingMsg{{Role: "user", Content: json.RawMessage(`"` + probe + `"`)}},
		raw: map[string]any{
			"model":    target.Model,
			"messages": []map[string]any{{"role": "user", "content": probe}},
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := s.upstream.completion(ctx, *target, req)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "latency_ms": lat})
		return
	}

	text := extractChoiceText(res)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"text":              text,
		"latency_ms":        lat,
		"prompt_tokens":     res.promptTokens,
		"completion_tokens": res.completionTokens,
	})
}

func (s *server) handleUsage(w http.ResponseWriter, r *http.Request) {
	usage, recent := s.store.usageSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{"usage": usage, "recent": recent})
}

func (s *server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": s.store.listAPIKeys()})
}

func (s *server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	key := s.store.createAPIKey(strings.TrimSpace(req.Name))
	writeJSON(w, http.StatusCreated, key)
}

func (s *server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.store.revokeAPIKey(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- Admin: static site ----------

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "web/index.html"
	} else if !strings.HasPrefix(p, "web/") {
		p = "web/" + p
	}
	data, err := webFS.ReadFile(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := "text/html; charset=utf-8"
	switch {
	case strings.HasSuffix(p, ".js"):
		contentType = "text/javascript; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		contentType = "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".svg"):
		contentType = "image/svg+xml"
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

// ---------- Middleware & helpers ----------

func (s *server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *server) authorized(r *http.Request) bool {
	if c, err := r.Cookie("session"); err == nil && s.store.validateSession(c.Value) {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return s.store.validateSession(strings.TrimPrefix(auth, "Bearer "))
	}
	return false
}

func (s *server) apiKeyFromRequest(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

func validateProvider(p *Provider) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name is required")
	}
	switch p.Type {
	case TypeOpenAI, TypeAnthropic, TypeGemini:
	default:
		return fmt.Errorf("invalid provider type %q", p.Type)
	}
	if p.Type == TypeOpenAI && strings.TrimSpace(p.BaseURL) == "" {
		return errors.New("base_url is required for OpenAI-compatible providers")
	}
	return nil
}

func extractChoiceText(res *completionResult) string {
	if res.body == nil {
		return ""
	}
	data, err := json.Marshal(res.body)
	if err != nil {
		return ""
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ""
	}
	if len(out.Choices) > 0 {
		return out.Choices[0].Message.Content
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "upstream_error",
			"code":    status,
		},
	})
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key")
}

func httpFlusher(w http.ResponseWriter) http.Flusher {
	f, _ := w.(http.Flusher)
	return f
}
