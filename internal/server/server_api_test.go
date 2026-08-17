package server_test

// Endpoint-surface tests for the wave-4 API additions: the Responses API
// (/v1/responses, #92), the Anthropic Messages API (/v1/messages, #57),
// the structured embeddings rejection (#47), CORS + OPTIONS preflight
// (#78), and the SSE keepalive/grace-flush/late-failure relay (#83/#59).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newTestServerWithLogger builds the full stack like newTestServer but with
// a custom logger (logring-wrapped) so tests can assert on trace entries.
func newTestServerWithLogger(t *testing.T, apiKeys []string, logger *slog.Logger, ring *logring.Handler, mocks ...*testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         make([]string, len(mocks)),
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		APIKeys:            apiKeys,
	}
	clients := make([]*upstream.Client, 0, len(mocks))
	sessions := make([]*session.Manager, 0, len(mocks))
	for i, mock := range mocks {
		cfg.AuthTokens[i] = fmt.Sprintf("tok-%d", i)
		clientCfg := *cfg
		clientCfg.UpstreamBaseURL = mock.URL()
		client, err := upstream.New(cfg.AuthTokens[i], &clientCfg)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, clients, sessions, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, logger, ring, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

// --- #78 CORS + OPTIONS preflight ---

func TestCORSPreflight204(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodOptions, ts.URL+"/v1/chat/completions", nil,
		map[string]string{"Origin": "https://app.example.com"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	headers := resp.Header.Get("Access-Control-Allow-Headers")
	for _, want := range []string{"Content-Type", "Authorization", "x-api-key", "anthropic-version"} {
		if !strings.Contains(headers, want) {
			t.Errorf("Access-Control-Allow-Headers missing %q: %q", want, headers)
		}
	}
	for _, want := range []string{"POST", "GET", "OPTIONS"} {
		if !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), want) {
			t.Errorf("Access-Control-Allow-Methods missing %q", want)
		}
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (preflight answered before routing)", mock.Requests)
	}
}

func TestCORSConfiguredOrigin(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.CORSAllowedOrigin = "https://app.example.com"
	}, mock)

	// Preflight echoes the pinned origin and varies on it.
	resp, _ := doJSON(t, http.MethodOptions, ts.URL+"/v1/models", nil,
		map[string]string{"Origin": "https://other.example.com"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want configured origin", got)
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Origin") {
		t.Errorf("Vary missing Origin: %q", resp.Header.Get("Vary"))
	}

	// A real /v1/* response carries the CORS headers too.
	resp2, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d: %s", resp2.StatusCode, truncate(string(data), 200))
	}
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("GET Access-Control-Allow-Origin = %q, want configured origin", got)
	}
}

func TestCORSPreflightNotAppliedToAdmin(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	// Admin routes are intentionally outside the CORS surface (cookie
	// auth); an OPTIONS there must not gain allow headers.
	resp, _ := doJSON(t, http.MethodOptions, ts.URL+"/admin/login", nil,
		map[string]string{"Origin": "https://evil.example.com"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("admin OPTIONS got Access-Control-Allow-Origin %q, want none", got)
	}
	if resp.StatusCode == http.StatusNoContent {
		t.Error("admin OPTIONS answered 204; want the mux's 405/404 (CORS must not intercept admin)")
	}
}

// --- #47 embeddings rejection ---

func TestEmbeddingsUnsupported(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/embeddings",
		[]byte(`{"input":"hello","model":"text-embedding-3"}`), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if body.Error.Type != "unsupported_endpoint" || body.Error.Code != "unsupported_endpoint" {
		t.Errorf("error type/code = %q/%q, want unsupported_endpoint", body.Error.Type, body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "/v1/chat/completions") {
		t.Errorf("message missing chat-completions pointer: %q", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, modelA) {
		t.Errorf("message missing model list (want %q): %q", modelA, body.Error.Message)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0 (rejected before pool)", mock.Requests)
	}
}

// --- #92 Responses API ---

// responsesChunks renders a text-only upstream chat stream ending with a
// usage block (so relayers surface upstream token accounting).
func responsesChunks() string {
	chunks := []string{
		chunk("chatcmpl-r1", 100, `"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`),
		chunk("chatcmpl-r1", 100, `"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]`),
		chunk("chatcmpl-r1", 100, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}`),
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(testutil.SSEEvent(c))
	}
	return sb.String()
}

func TestResponsesNonStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","instructions":"be brief","input":"ping"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Status  string `json:"status"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Errorf("object/status = %q/%q, want response/completed", out.Object, out.Status)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" {
		t.Fatalf("output = %+v, want one message item", out.Output)
	}
	msg := out.Output[0]
	if msg.Role != "assistant" || msg.Status != "completed" {
		t.Errorf("message role/status = %q/%q, want assistant/completed", msg.Role, msg.Status)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello world" {
		t.Errorf("content = %+v, want single output_text 'Hello world'", msg.Content)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("message id = %q, want msg_ prefix", msg.ID)
	}
	if out.Usage.OutputTokens == 0 {
		t.Error("usage.output_tokens = 0, want upstream usage surfaced")
	}
	if !mock.BodyContains(`"role":"system"`) {
		t.Error("upstream chat body missing the instructions system message")
	}
	if !mock.BodyContains("be brief") || !mock.BodyContains("ping") {
		t.Error("upstream chat body missing instructions/input content")
	}
}

func TestResponsesStringInput(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"hello"}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if !mock.BodyContains(`"role":"user"`) || !mock.BodyContains("hello") {
		t.Error("string input not converted to a user message")
	}
}

func TestResponsesToolCallNonStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r2", 101,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-r2", 101, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"weather?","tools":[{"type":"function","name":"get_weather","description":"weather lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"function","name":"get_weather"}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "function_call" {
		t.Fatalf("output = %+v, want one function_call", out.Output)
	}
	fc := out.Output[0]
	if fc.Name != "get_weather" || !strings.Contains(fc.Arguments, `"city":"SF"`) {
		t.Errorf("function_call name/arguments = %q/%q, want get_weather with city SF", fc.Name, fc.Arguments)
	}
	if fc.CallID != "call_abc" {
		t.Errorf("call_id = %q, want upstream call_abc", fc.CallID)
	}
	// The upstream body must carry the wrapped function tool + translated
	// tool_choice.
	if !mock.BodyContains(`"type":"function"`) || !mock.BodyContains(`"get_weather"`) {
		t.Error("upstream body missing function-wrapped tool")
	}
	if !mock.BodyContains(`"tool_choice"`) || !mock.BodyContains(`"name":"get_weather"`) {
		t.Error("upstream body missing translated tool_choice")
	}
}

func TestResponsesStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"ping","stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	sse := string(data)
	if !strings.Contains(sse, ": connecting") {
		t.Error("stream missing ': connecting' grace-flush comment")
	}
	for _, want := range []string{
		`"type":"response.created"`,
		`"type":"response.in_progress"`,
		`"type":"response.output_item.added"`,
		`"type":"response.content_part.added"`,
		`"type":"response.output_text.delta"`,
		`"delta":"Hello"`,
		`"type":"response.output_text.done"`,
		`"type":"response.content_part.done"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
		`"status":"completed"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	// Terminal: response.completed carries the assembled text and usage.
	if !strings.Contains(sse, `"text":"Hello world"`) {
		t.Error("response.completed missing assembled output text")
	}
}

func TestResponsesStreamToolCall(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r3", 102,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"search_web","arguments":"{\"q\":\"freebuff\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-r3", 102, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","input":"search","tools":[{"type":"function","name":"search_web","description":"s","parameters":{"type":"object","properties":{}}}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/responses", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	for _, want := range []string{
		`"type":"response.output_item.added"`,
		`"type":"function_call"`,
		`"type":"response.function_call_arguments.delta"`,
		`"delta":"{\"q\":\"freebuff\"}"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	if !strings.Contains(sse, `"name":"search_web"`) {
		t.Error("completed function_call missing the tool name")
	}
}

// --- #57 Anthropic Messages API ---

// anthropicTextChunks renders an upstream chat stream with reasoning +
// text + stop + a usage block.
func anthropicTextChunks() string {
	chunks := []string{
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{"reasoning_content":"think step by step"},"finish_reason":null}]`),
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]`),
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{"content":" there"},"finish_reason":null}]`),
		chunk("chatcmpl-a1", 200, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":4,"total_tokens":25}`),
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(testutil.SSEEvent(c))
	}
	return sb.String()
}

func TestMessagesNonStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = anthropicTextChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","max_tokens":200,"system":"You are helpful.","messages":[{"role":"user","content":"hi"}],"stream":false}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body),
		map[string]string{"x-api-key": "k", "anthropic-version": "2023-06-01"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var msg struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		Usage map[string]int64 `json:"usage"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("message not JSON: %v", err)
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("type/role = %q/%q, want message/assistant", msg.Type, msg.Role)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if len(msg.Content) != 2 || msg.Content[0].Type != "thinking" || msg.Content[1].Type != "text" {
		t.Fatalf("content = %+v, want thinking then text blocks", msg.Content)
	}
	if msg.Content[0].Thinking != "think step by step" {
		t.Errorf("thinking block = %q", msg.Content[0].Thinking)
	}
	if msg.Content[1].Text != "Hello there" {
		t.Errorf("text block = %q, want 'Hello there'", msg.Content[1].Text)
	}
	if msg.Usage["output_tokens"] == 0 {
		t.Error("usage.output_tokens = 0, want upstream usage")
	}
	// Request conversion: the upstream body carries the system message,
	// the user message, and the resolved model.
	if !mock.BodyContains(`"role":"system"`) || !mock.BodyContains("You are helpful.") {
		t.Error("upstream body missing system message")
	}
	if !mock.BodyContains(`"role":"user"`) || !mock.BodyContains("hi") {
		t.Error("upstream body missing user message")
	}
	if !mock.BodyContains(modelA) {
		t.Error("upstream body missing resolved model")
	}
}

func TestMessagesToolUseRoundTrip(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-a2", 201,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_01","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-a2", 201, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"weather?"}],"tools":[{"name":"get_weather","description":"weather lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"get_weather"}}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var msg struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("message not JSON: %v", err)
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v, want one tool_use block", msg.Content)
	}
	tu := msg.Content[0]
	if tu.Name != "get_weather" || tu.Input["city"] != "SF" {
		t.Errorf("tool_use name/input = %q/%v, want get_weather/SF", tu.Name, tu.Input)
	}
	if tu.ID != "toolu_01" {
		t.Errorf("tool_use id = %q, want toolu_01 preserved", tu.ID)
	}
	// Upstream body: function-wrapped tool + translated tool_choice.
	if !mock.BodyContains(`"type":"function"`) || !mock.BodyContains(`"get_weather"`) {
		t.Error("upstream body missing wrapped tool")
	}
	if !mock.BodyContains(`"tool_choice"`) || !mock.BodyContains(`"name":"get_weather"`) {
		t.Error("upstream body missing translated tool_choice")
	}
}

func TestMessagesToolResultConversion(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[
		{"role":"assistant","content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"toolu_99","name":"get_weather","input":{"city":"SF"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_99","content":"sunny, 70F"},{"type":"text","text":"thanks"}]}
	]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	recorded := mock.RecordedChatBodies
	if len(recorded) == 0 {
		t.Fatal("no upstream chat body recorded")
	}
	up := recorded[0]
	for _, want := range []string{
		`"tool_call_id":"toolu_99"`,
		`"role":"tool"`,
		"sunny, 70F",
		`"role":"assistant"`,
		`"tool_calls"`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("upstream body missing %s: %s", want, truncate(up, 500))
		}
	}
}

func TestMessagesStream(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = anthropicTextChunks()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	sse := string(data)
	// Event sequence: message_start → thinking block → text block →
	// message_delta → message_stop.
	for _, want := range []string{
		`"type":"message_start"`,
		`"type":"content_block_start"`,
		`"type":"thinking"`,
		`"type":"thinking_delta"`,
		`"thinking":"think step by step"`,
		`"type":"text_delta"`,
		`"text":"Hello"`,
		`"type":"signature_delta"`,
		`"type":"content_block_stop"`,
		`"type":"message_delta"`,
		`"stop_reason":"end_turn"`,
		`"type":"message_stop"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
	if !strings.Contains(sse, `"model":"`+modelA+`"`) {
		t.Error("message_start missing the requested anthropic model")
	}
}

func TestMessagesStreamToolUse(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-a3", 202,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_07","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-a3", 202, `"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`))
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"ls"}],"tools":[{"name":"run_shell","description":"run a shell command","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages", []byte(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	sse := string(data)
	for _, want := range []string{
		`"type":"content_block_start"`,
		`"type":"tool_use"`,
		`"name":"run_shell"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"cmd\":\"ls\"}"`,
		`"stop_reason":"tool_use"`,
		`"type":"message_stop"`,
	} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream missing %s", want)
		}
	}
}

func TestMessagesCountTokensUnsupported(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	body := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}]}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/messages/count_tokens", []byte(body), nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var out struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if out.Error.Type != "unsupported_endpoint" || out.Error.Code != "count_tokens_unsupported" {
		t.Errorf("type/code = %q/%q, want unsupported_endpoint/count_tokens_unsupported", out.Error.Type, out.Error.Code)
	}
	if mock.Requests != 0 {
		t.Errorf("upstream requests = %d, want 0", mock.Requests)
	}
}

// --- #83/#59 SSE keepalive, grace flush, late failure ---

// TestRelayStreamInBandErrorFrame verifies an upstream error chunk is
// relayed in-band (type upstream_error + provider message) followed by
// [DONE] — the reference late-failure contract.
func TestRelayStreamInBandErrorFrame(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-e1","object":"chat.completion.chunk","created":400,"model":"`+modelA+`","choices":[{"index":0,"delta":{},"finish_reason":null}]}`) +
		testutil.SSEEvent(`{"error":{"message":"Upstream provider error (429): Model is at capacity.","type":"upstream_error"}}`)
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error delivered in-band): %s", resp.StatusCode, truncate(string(data), 200))
	}
	body := string(data)
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Error("error frame missing type upstream_error")
	}
	if !strings.Contains(body, "Upstream provider error (429): Model is at capacity.") {
		t.Error("error frame missing the provider message")
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("stream missing the [DONE] terminator after the error frame")
	}
}

// --- #89 phase timing in traces ---

// TestTracePhasesRecorded verifies the chat trace carries the per-request
// latency phases (acquire_ms/upstream_ttfb_ms/total_ms; session/run phases
// come from the pool when it opts into phasetiming).
func TestTracePhasesRecorded(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = responsesChunks()
	var sink bytes.Buffer
	ring := logring.NewHandler(slog.NewTextHandler(&sink, nil), 200)
	ts, _ := newTestServerWithLogger(t, nil, slog.New(ring), ring, mock)

	_, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if !strings.Contains(string(data), "Hello") {
		t.Fatalf("chat stream unexpected: %s", truncate(string(data), 200))
	}
	entries := ring.Recent(50)
	var trace *logring.Entry
	for i := range entries {
		if entries[i].Message == "chat trace" {
			trace = &entries[i]
			break
		}
	}
	if trace == nil {
		t.Fatal("no 'chat trace' entry in the log ring")
	}
	joined := strings.Join(trace.Fields, " ")
	for _, phase := range []string{"acquire_ms", "upstream_ttfb_ms", "total_ms"} {
		re := regexp.MustCompile(phase + `=\d+`)
		if !re.MatchString(joined) {
			t.Errorf("trace fields missing %s: %s", phase, joined)
		}
	}
	if !strings.Contains(joined, "status=ok") {
		t.Errorf("trace missing status=ok: %s", joined)
	}
}
