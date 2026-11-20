package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/opencode"
	"github.com/Autumn-27/norma/llm"
)

// mcpTestHandler builds a root mux with only the MCP bridge routes mounted,
// mirroring how Handler() mounts them (outside the JWT-wrapped /api/ subtree).
func mcpTestHandler(s *Server) http.Handler {
	root := http.NewServeMux()
	s.registerMCPRoutes(root)
	return root
}

func TestMCPTokenAuth(t *testing.T) {
	t.Setenv("ARTEX_MCP_TOKEN", "test-mcp-token")
	s := &Server{}
	h := mcpTestHandler(s)

	cases := []struct {
		header string
		want   int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer test-mcp-token", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/api/mcp/status", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Fatalf("auth %q: status=%d want %d body=%s", c.header, w.Code, c.want, w.Body.String())
		}
	}
}

func TestMCPBridgeLLMRoundtrip(t *testing.T) {
	t.Setenv("ARTEX_MCP_TOKEN", "test-mcp-token")
	s := &Server{}
	h := mcpTestHandler(s)

	// One parked provider call: the planner side of the bridge.
	prov := opencode.NewProvider("test-model")
	type result struct {
		msg  llm.Message
		stop string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		msg, stop, _, err := prov.Complete(context.Background(), llm.CompletionRequest{
			System:   []string{"You are the planner."},
			Messages: []llm.Message{llm.UserText("plan something")},
		})
		resCh <- result{msg, stop, err}
	}()

	// Operator: fetch the request.
	req := httptest.NewRequest("POST", "/api/mcp/llm/next", strings.NewReader(`{"wait_seconds":5}`))
	req.Header.Set("Authorization", "Bearer test-mcp-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("next: status=%d body=%s", w.Code, w.Body.String())
	}
	var next struct {
		Request *opencode.Payload `json:"request"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatalf("next body: %v", err)
	}
	if next.Request == nil {
		t.Fatal("no request arrived at the queue")
	}
	if len(next.Request.System) != 1 || next.Request.System[0] != "You are the planner." {
		t.Fatalf("request system = %+v", next.Request.System)
	}
	if next.Request.InputEstTokens == 0 {
		t.Fatal("input estimate missing")
	}

	// Operator: submit the assistant turn (tool call without an id — the
	// bridge must synthesize one — and with stringified input, tolerated).
	body := `{"request_id":"` + next.Request.ID + `","blocks":[` +
		`{"type":"text","text":"running the tool"},` +
		`{"type":"tool_use","name":"bash","input":"{\"cmd\":\"id\"}"}]}`
	req = httptest.NewRequest("POST", "/api/mcp/llm/submit", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-mcp-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("submit: status=%d body=%s", w.Code, w.Body.String())
	}

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("provider error: %v", res.err)
		}
		if res.stop != "tool_use" {
			t.Fatalf("stop=%q", res.stop)
		}
		if len(res.msg.ToolUses()) != 1 || res.msg.ToolUses()[0].ID == "" {
			t.Fatalf("tool uses = %+v", res.msg.ToolUses())
		}
		if got := string(res.msg.ToolUses()[0].Input); got != `{"cmd":"id"}` {
			t.Fatalf("tool input = %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider call did not return after submit")
	}

	// A second submit for the same request must fail: exactly-once delivery.
	req = httptest.NewRequest("POST", "/api/mcp/llm/submit", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-mcp-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("double submit: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMCPSubmitValidation(t *testing.T) {
	t.Setenv("ARTEX_MCP_TOKEN", "test-mcp-token")
	s := &Server{}
	h := mcpTestHandler(s)

	// Park one request so a submit has something to target.
	call := opencode.Default.Enqueue(opencode.Payload{})
	defer call.Cancel()
	p, ok := opencode.Default.Next(context.Background(), time.Second)
	if !ok {
		t.Fatal("Next found nothing")
	}
	if p.ID != call.ID() {
		t.Fatalf("Next id %q != enqueued %q", p.ID, call.ID())
	}

	post := func(body string) int {
		req := httptest.NewRequest("POST", "/api/mcp/llm/submit", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-mcp-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := post(`{"request_id":"` + p.ID + `","blocks":[{"type":"tool_use","name":"b","input":"no json"}]}`); code != 400 {
		t.Fatalf("invalid tool input: status=%d", code)
	}
	if code := post(`{"request_id":"","blocks":[]}`); code != 400 {
		t.Fatalf("empty submit: status=%d", code)
	}
	if code := post(`{"request_id":"oc-does-not-exist","blocks":[{"type":"text","text":"x"}]}`); code != 404 {
		t.Fatalf("unknown id: status=%d", code)
	}
}

func TestMCPNextEmpty(t *testing.T) {
	t.Setenv("ARTEX_MCP_TOKEN", "test-mcp-token")
	s := &Server{}
	h := mcpTestHandler(s)

	req := httptest.NewRequest("POST", "/api/mcp/llm/next", strings.NewReader(`{"wait_seconds":0}`))
	req.Header.Set("Authorization", "Bearer test-mcp-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"request":null`) {
		t.Fatalf("expected null request, got %s", w.Body.String())
	}
}
