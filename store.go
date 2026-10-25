package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// ProviderType identifies the API dialect an upstream provider speaks.
type ProviderType string

const (
	TypeOpenAI    ProviderType = "openai"    // OpenAI-compatible chat completions (Hack Club AI, OpenRouter, ...)
	TypeAnthropic ProviderType = "anthropic" // Anthropic Messages API (Claude)
	TypeGemini    ProviderType = "gemini"    // Google Gemini generateContent API
)

// Provider is a single upstream LLM provider. Providers are tried in list order.
type Provider struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Type    ProviderType `json:"type"`
	BaseURL string       `json:"base_url"`
	Model   string       `json:"model"`
	APIKey  string       `json:"api_key"`
	Enabled bool         `json:"enabled"`
}

// UsageStats aggregates per-provider request metrics.
type UsageStats struct {
	Name             string `json:"name"`
	Requests         int64  `json:"requests"`
	Successes        int64  `json:"successes"`
	Failures         int64  `json:"failures"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	TotalLatencyMs   int64  `json:"total_latency_ms"`
	LastUsedUnix     int64  `json:"last_used_unix"`
}

// RequestRecord is a single logged request (kept in a bounded ring buffer).
type RequestRecord struct {
	TimeUnix         int64  `json:"time_unix"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Status           string `json:"status"` // "success" | "error"
	LatencyMs        int64  `json:"latency_ms"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Error            string `json:"error,omitempty"`
}

// Store holds all runtime state in memory.
type Store struct {
	mu             sync.RWMutex
	masterPassword string
	providers      []*Provider
	usage          map[string]*UsageStats
	recent         []RequestRecord
	sessions       map[string]time.Time
}

func newStore(masterPassword string) *Store {
	return &Store{
		masterPassword: masterPassword,
		providers:      []*Provider{},
		usage:          map[string]*UsageStats{},
		recent:         []RequestRecord{},
		sessions:       map[string]time.Time{},
	}
}

func (s *Store) checkPassword(pw string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return pw != "" && pw == s.masterPassword
}

func (s *Store) listProviders() []Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Provider, len(s.providers))
	for i, p := range s.providers {
		out[i] = *p
	}
	return out
}

func (s *Store) enabledProviders() []Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Provider{}
	for _, p := range s.providers {
		if p.Enabled {
			out = append(out, *p)
		}
	}
	return out
}

func (s *Store) addProvider(p Provider) Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = newID()
	}
	s.providers = append(s.providers, &p)
	return p
}

func (s *Store) updateProvider(id string, p Provider) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.providers {
		if cur.ID == id {
			p.ID = id
			s.providers[i] = &p
			return p, true
		}
	}
	return Provider{}, false
}

func (s *Store) deleteProvider(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.providers {
		if p.ID == id {
			s.providers = append(s.providers[:i], s.providers[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Store) reorderProviders(ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) != len(s.providers) {
		return false
	}
	byID := map[string]*Provider{}
	for _, p := range s.providers {
		byID[p.ID] = p
	}
	reordered := make([]*Provider, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		p, ok := byID[id]
		if !ok || seen[id] {
			return false
		}
		seen[id] = true
		reordered = append(reordered, p)
	}
	s.providers = reordered
	return true
}

func (s *Store) recordUsage(providerID, name, model, status string, promptTokens, completionTokens, latencyMs int64, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.usage[providerID]
	if st == nil {
		st = &UsageStats{}
		s.usage[providerID] = st
	}
	st.Name = name
	st.Requests++
	if status == "success" {
		st.Successes++
	} else {
		st.Failures++
	}
	st.PromptTokens += promptTokens
	st.CompletionTokens += completionTokens
	st.TotalTokens += promptTokens + completionTokens
	st.TotalLatencyMs += latencyMs
	st.LastUsedUnix = time.Now().Unix()

	rec := RequestRecord{
		TimeUnix:         time.Now().Unix(),
		Provider:         name,
		Model:            model,
		Status:           status,
		LatencyMs:        latencyMs,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		Error:            errMsg,
	}
	s.recent = append([]RequestRecord{rec}, s.recent...)
	if len(s.recent) > 500 {
		s.recent = s.recent[:500]
	}
}

func (s *Store) usageSnapshot() (map[string]*UsageStats, []RequestRecord) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := map[string]*UsageStats{}
	for k, v := range s.usage {
		cp := *v
		m[k] = &cp
	}
	recent := make([]RequestRecord, len(s.recent))
	copy(recent, s.recent)
	return m, recent
}

func (s *Store) createSession(ttl time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := newID()
	s.sessions[tok] = time.Now().Add(ttl)
	return tok
}

func (s *Store) validateSession(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, tok)
		return false
	}
	return true
}

func (s *Store) deleteSession(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, tok)
}

func (s *Store) cleanupSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
