package mcpbridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeARTEX stands in for the backend: next returns one fixed request, submit
// records what arrived.
func fakeARTEX(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/mcp/llm/next":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request": map[string]any{"id": "oc-1", "system": []string{"be the planner"}, "messages": []any{}, "input_est_tokens": 42},
			})
		case "/api/mcp/llm/submit":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// rpc runs one Serve session against scripted stdin lines and returns the
// decoded responses in order.
func rpc(t *testing.T, lines []string) []map[string]any {
	t.Helper()
	in := bytes.NewReader([]byte(strings.Join(lines, "\n") + "\n"))
	var out bytes.Buffer
	if err := Serve(&Client{}, "test", in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resps = append(resps, m)
	}
	return resps
}

func TestServeInitializeAndToolsList(t *testing.T) {
	resps := rpc(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	})
	if len(resps) != 3 {
		t.Fatalf("responses = %d (notifications must not be answered)", len(resps))
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", init["protocolVersion"])
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(ToolList) {
		t.Fatalf("tools = %d, want %d", len(tools), len(ToolList))
	}
	if _, ok := resps[2]["result"]; !ok {
		t.Fatalf("ping result missing: %v", resps[2])
	}
}

func TestToolsCallRoundtrip(t *testing.T) {
	fake, got := fakeARTEX(t)
	c := NewClient(fake.URL, "test-token")

	next, err := toolByName("artex_llm_next").Call(c, []byte(`{"wait_seconds":1}`))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	var nextBody struct {
		Request struct {
			ID string `json:"id"`
		} `json:"request"`
	}
	if err := json.Unmarshal(next.(json.RawMessage), &nextBody); err != nil {
		t.Fatalf("next body: %v", err)
	}
	if nextBody.Request.ID != "oc-1" {
		t.Fatalf("request id = %q", nextBody.Request.ID)
	}
	reqID := nextBody.Request.ID

	_, err = toolByName("artex_llm_submit").Call(c, []byte(`{"request_id":"`+reqID+`","blocks":[{"type":"text","text":"done"}]}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if (*got)["request_id"] != "oc-1" {
		t.Fatalf("submit body = %v", *got)
	}
	if b, ok := (*got)["blocks"].([]any); !ok || len(b) != 1 {
		t.Fatalf("blocks not passed through verbatim: %v", (*got)["blocks"])
	}
}

func TestToolsCallErrorIsToolResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "test-token")

	res, rpcErr := handle(c, "test", ToolList, rpcMessage{
		Method: "tools/call",
		Params: []byte(`{"name":"artex_status","arguments":{}}`),
	})
	if rpcErr != nil {
		t.Fatalf("http failure must be a tool result, not an rpc error: %v", rpcErr)
	}
	m := res.(map[string]any)
	if m["isError"] != true {
		t.Fatalf("result = %v", m)
	}
}

func TestToolCallArgValidation(t *testing.T) {
	fake, _ := fakeARTEX(t)
	c := NewClient(fake.URL, "test-token")
	if _, err := toolByName("artex_task").Call(c, []byte(`{}`)); err == nil {
		t.Fatal("missing id must error")
	}
	if _, err := toolByName("artex_task").Call(c, []byte(`{"id":""}`)); err == nil {
		t.Fatal("empty id must error")
	}
	if _, err := argString([]byte(`{"id":12}`), "id"); err != nil {
		t.Fatalf("numeric id should coerce: %v", err)
	}
}

func TestQueryString(t *testing.T) {
	got := queryString([]byte(`{"task_id":"3","limit":10,"ignored":"x"}`), "task_id", "limit")
	if got != "?task_id=3&limit=10" {
		t.Fatalf("query = %q", got)
	}
	if s := queryString(nil, "task_id"); s != "" {
		t.Fatalf("empty args query = %q", s)
	}
}

func toolByName(name string) Tool {
	for _, t := range ToolList {
		if t.Name == name {
			return t
		}
	}
	panic("missing tool " + name)
}
