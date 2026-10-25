package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default base URLs when a provider leaves base_url empty.
const (
	defaultAnthropicBaseURL = "https://api.anthropic.com/v1"
	defaultGeminiBaseURL    = "https://generativelanguage.googleapis.com/v1beta"
)

// incomingRequest is the normalized OpenAI-compatible request we receive.
type incomingRequest struct {
	Model               string          `json:"model"`
	Messages            []incomingMsg   `json:"messages"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	raw                 map[string]any
}

type incomingMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type completionResult struct {
	rawBody          []byte // for OpenAI passthrough (returned verbatim)
	body             map[string]any
	promptTokens     int64
	completionTokens int64
}

type streamUsage struct {
	promptTokens     int64
	completionTokens int64
}

type upstreamError struct {
	status int
	body   string
}

func (e *upstreamError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
	}
	return e.body
}

type upstream struct {
	hc *http.Client
}

func newUpstream() *upstream {
	return &upstream{
		hc: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func parseIncoming(body []byte) (*incomingRequest, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	var req incomingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	req.raw = raw
	if len(req.Messages) == 0 {
		return nil, errors.New("messages array is required")
	}
	return &req, nil
}

func resolveModel(p Provider, req *incomingRequest) string {
	if p.Model != "" {
		return p.Model
	}
	return req.Model
}

// completion runs a non-streaming completion against a provider and returns an
// OpenAI-formatted response.
func (u *upstream) completion(ctx context.Context, p Provider, req *incomingRequest) (*completionResult, error) {
	switch p.Type {
	case TypeAnthropic:
		return u.anthropicCompletion(ctx, p, req)
	case TypeGemini:
		return u.geminiCompletion(ctx, p, req)
	default:
		return u.openAICompletion(ctx, p, req)
	}
}

// streamCompletion streams an OpenAI-format SSE response to the client. It
// returns an error only while it is still safe to fail over (i.e. nothing has
// been written to w). Once it commits to a provider it returns nil.
func (u *upstream) streamCompletion(ctx context.Context, p Provider, req *incomingRequest, w http.ResponseWriter, flusher http.Flusher) (*streamUsage, error) {
	switch p.Type {
	case TypeAnthropic:
		return u.anthropicStream(ctx, p, req, w, flusher)
	case TypeGemini:
		return u.geminiStream(ctx, p, req, w, flusher)
	default:
		return u.openAIStream(ctx, p, req, w, flusher)
	}
}

// ---------- OpenAI-compatible ----------

func (u *upstream) openAICompletion(ctx context.Context, p Provider, req *incomingRequest) (*completionResult, error) {
	body := cloneMap(req.raw)
	body["model"] = resolveModel(p, req)
	body["stream"] = false
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := u.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &upstreamError{status: resp.StatusCode, body: truncate(string(respBody), 500)}
	}

	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("provider returned invalid JSON: %w", err)
	}
	pt, ct := extractUsage(out)
	return &completionResult{rawBody: respBody, promptTokens: pt, completionTokens: ct}, nil
}

func (u *upstream) openAIStream(ctx context.Context, p Provider, req *incomingRequest, w http.ResponseWriter, flusher http.Flusher) (*streamUsage, error) {
	body := cloneMap(req.raw)
	body["model"] = resolveModel(p, req)
	body["stream"] = true
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := u.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &upstreamError{status: resp.StatusCode, body: truncate(string(b), 500)}
	}

	// Committed: upstream is streaming. From here on we cannot fail over.
	beginSSE(w)
	_ = copyStream(w, flusher, resp.Body)
	return &streamUsage{}, nil
}

// ---------- Anthropic ----------

type anthropicMessage struct {
	ID         string `json:"id"`
	Role       string `json:"role"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (u *upstream) anthropicCompletion(ctx context.Context, p Provider, req *incomingRequest) (*completionResult, error) {
	body := buildAnthropicBody(p, req, false)
	data, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicBase(p)+"/messages", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := u.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &upstreamError{status: resp.StatusCode, body: truncate(string(respBody), 500)}
	}

	var am anthropicMessage
	if err := json.Unmarshal(respBody, &am); err != nil {
		return nil, fmt.Errorf("provider returned invalid JSON: %w", err)
	}
	return &completionResult{
		body:             anthropicToOpenAI(&am, resolveModel(p, req)),
		promptTokens:     am.Usage.InputTokens,
		completionTokens: am.Usage.OutputTokens,
	}, nil
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Message *struct {
		Usage *struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (u *upstream) anthropicStream(ctx context.Context, p Provider, req *incomingRequest, w http.ResponseWriter, flusher http.Flusher) (*streamUsage, error) {
	body := buildAnthropicBody(p, req, true)
	data, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicBase(p)+"/messages", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := u.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &upstreamError{status: resp.StatusCode, body: truncate(string(b), 500)}
	}

	beginSSE(w)
	model := resolveModel(p, req)
	id := "chatcmpl-" + newID()
	usage := streamUsage{}
	stopReason := ""

	_ = writeSSE(w, flusher, chatChunk(id, model, map[string]any{"role": "assistant", "content": ""}, nil))

	_ = scanSSE(resp.Body, func(ev sseEvent) error {
		var e anthropicStreamEvent
		if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
			return nil
		}
		switch e.Type {
		case "message_start":
			if e.Message != nil && e.Message.Usage != nil {
				usage.promptTokens = e.Message.Usage.InputTokens
			}
		case "content_block_delta":
			if e.Delta != nil && e.Delta.Type == "text_delta" && e.Delta.Text != "" {
				_ = writeSSE(w, flusher, chatChunk(id, model, map[string]any{"content": e.Delta.Text}, nil))
			}
		case "message_delta":
			if e.Delta != nil && e.Delta.StopReason != "" {
				stopReason = e.Delta.StopReason
			}
			if e.Usage != nil {
				usage.completionTokens = e.Usage.OutputTokens
			}
		}
		return nil
	})

	finish := anthropicFinish(stopReason)
	_ = writeSSE(w, flusher, chatChunk(id, model, map[string]any{}, &finish))
	writeDone(w, flusher)
	return &usage, nil
}

func buildAnthropicBody(p Provider, req *incomingRequest, stream bool) map[string]any {
	var systemParts []string
	msgs := []map[string]any{}
	for _, m := range req.Messages {
		content := contentToString(m.Content)
		switch m.Role {
		case "system", "developer":
			systemParts = append(systemParts, content)
		default:
			role := m.Role
			if role != "user" && role != "assistant" {
				role = "user"
			}
			msgs = append(msgs, map[string]any{"role": role, "content": content})
		}
	}
	maxTokens := 1024
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		maxTokens = *req.MaxCompletionTokens
	}
	body := map[string]any{
		"model":      resolveModel(p, req),
		"max_tokens": maxTokens,
		"messages":   msgs,
		"stream":     stream,
	}
	if len(systemParts) > 0 {
		body["system"] = strings.Join(systemParts, "\n")
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	return body
}

func anthropicBase(p Provider) string {
	if strings.TrimSpace(p.BaseURL) != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return defaultAnthropicBaseURL
}

func anthropicToOpenAI(am *anthropicMessage, model string) map[string]any {
	var parts []string
	for _, c := range am.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	content := strings.Join(parts, "")
	total := am.Usage.InputTokens + am.Usage.OutputTokens
	return map[string]any{
		"id":      "chatcmpl-" + am.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": anthropicFinish(am.StopReason),
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     am.Usage.InputTokens,
			"completion_tokens": am.Usage.OutputTokens,
			"total_tokens":      total,
		},
	}
}

func anthropicFinish(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// ---------- Gemini ----------

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
			Role string `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func (u *upstream) geminiCompletion(ctx context.Context, p Provider, req *incomingRequest) (*completionResult, error) {
	body := buildGeminiBody(p, req)
	data, _ := json.Marshal(body)
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", geminiBase(p), url.PathEscape(resolveModel(p, req)))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("x-goog-api-key", p.APIKey)
	}

	resp, err := u.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &upstreamError{status: resp.StatusCode, body: truncate(string(respBody), 500)}
	}

	var gm geminiResponse
	if err := json.Unmarshal(respBody, &gm); err != nil {
		return nil, fmt.Errorf("provider returned invalid JSON: %w", err)
	}
	return &completionResult{
		body:             geminiToOpenAI(&gm, resolveModel(p, req)),
		promptTokens:     gm.UsageMetadata.PromptTokenCount,
		completionTokens: gm.UsageMetadata.CandidatesTokenCount,
	}, nil
}

type geminiStreamChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

func (u *upstream) geminiStream(ctx context.Context, p Provider, req *incomingRequest, w http.ResponseWriter, flusher http.Flusher) (*streamUsage, error) {
	body := buildGeminiBody(p, req)
	data, _ := json.Marshal(body)
	endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", geminiBase(p), url.PathEscape(resolveModel(p, req)))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("x-goog-api-key", p.APIKey)
	}

	resp, err := u.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &upstreamError{status: resp.StatusCode, body: truncate(string(b), 500)}
	}

	beginSSE(w)
	model := resolveModel(p, req)
	id := "chatcmpl-" + newID()
	usage := streamUsage{}
	lastText := ""
	finish := ""
	roleSent := false

	_ = scanSSE(resp.Body, func(ev sseEvent) error {
		if ev.data == "[DONE]" {
			return nil
		}
		var chunk geminiStreamChunk
		if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
			return nil
		}
		if chunk.UsageMetadata != nil {
			usage.promptTokens = chunk.UsageMetadata.PromptTokenCount
			usage.completionTokens = chunk.UsageMetadata.CandidatesTokenCount
		}
		if len(chunk.Candidates) == 0 {
			return nil
		}
		cand := chunk.Candidates[0]
		var full string
		for _, part := range cand.Content.Parts {
			full += part.Text
		}
		if !roleSent {
			_ = writeSSE(w, flusher, chatChunk(id, model, map[string]any{"role": "assistant", "content": ""}, nil))
			roleSent = true
		}
		var delta string
		if strings.HasPrefix(full, lastText) {
			delta = full[len(lastText):]
		} else {
			delta = full
		}
		lastText = full
		if delta != "" {
			_ = writeSSE(w, flusher, chatChunk(id, model, map[string]any{"content": delta}, nil))
		}
		if cand.FinishReason != "" {
			finish = geminiFinish(cand.FinishReason)
		}
		return nil
	})

	_ = writeSSE(w, flusher, chatChunk(id, model, map[string]any{}, &finish))
	writeDone(w, flusher)
	return &usage, nil
}

func buildGeminiBody(p Provider, req *incomingRequest) map[string]any {
	var system string
	var contents []map[string]any
	for _, m := range req.Messages {
		text := contentToString(m.Content)
		switch m.Role {
		case "system", "developer":
			if system != "" {
				system += "\n"
			}
			system += text
		default:
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]any{
				"role":  role,
				"parts": []map[string]any{{"text": text}},
			})
		}
	}
	genConfig := map[string]any{}
	if req.Temperature != nil {
		genConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		genConfig["topP"] = *req.TopP
	}
	if req.MaxTokens != nil {
		genConfig["maxOutputTokens"] = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		genConfig["maxOutputTokens"] = *req.MaxCompletionTokens
	}
	body := map[string]any{"contents": contents}
	if len(genConfig) > 0 {
		body["generationConfig"] = genConfig
	}
	if system != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": system}}}
	}
	return body
}

func geminiBase(p Provider) string {
	if strings.TrimSpace(p.BaseURL) != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return defaultGeminiBaseURL
}

func geminiToOpenAI(gm *geminiResponse, model string) map[string]any {
	var text string
	var finish string
	if len(gm.Candidates) > 0 {
		for _, part := range gm.Candidates[0].Content.Parts {
			text += part.Text
		}
		finish = geminiFinish(gm.Candidates[0].FinishReason)
	}
	total := gm.UsageMetadata.TotalTokenCount
	if total == 0 {
		total = gm.UsageMetadata.PromptTokenCount + gm.UsageMetadata.CandidatesTokenCount
	}
	return map[string]any{
		"id":      "chatcmpl-" + newID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     gm.UsageMetadata.PromptTokenCount,
			"completion_tokens": gm.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      total,
		},
	}
}

func geminiFinish(s string) string {
	switch s {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MALFORMED_FUNCTION_CALL":
		return "content_filter"
	default:
		return "stop"
	}
}

// ---------- Helpers ----------

func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		var parts []string
		for _, it := range arr {
			if it.Text != "" {
				parts = append(parts, it.Text)
			}
		}
		return strings.Join(parts, "")
	}
	var generic []map[string]any
	if err := json.Unmarshal(raw, &generic); err == nil {
		var parts []string
		for _, it := range generic {
			if t, ok := it["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func extractUsage(body map[string]any) (int64, int64) {
	u, ok := body["usage"].(map[string]any)
	if !ok {
		return 0, 0
	}
	return int64Val(u["prompt_tokens"]), int64Val(u["completion_tokens"])
}

func int64Val(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- SSE helpers ----------

func beginSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func chatChunk(id, model string, delta map[string]any, finish *string) map[string]any {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
	}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	return chunk
}

func writeSSE(w io.Writer, flusher http.Flusher, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func writeDone(w io.Writer, flusher http.Flusher) {
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func copyStream(dst io.Writer, flusher http.Flusher, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

type sseEvent struct {
	event string
	data  string
}

func scanSSE(r io.Reader, fn func(sseEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var event string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		ev := sseEvent{event: event, data: strings.Join(dataLines, "\n")}
		event = ""
		dataLines = nil
		return fn(ev)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
