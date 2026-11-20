package mcpbridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// Tool describes one MCP tool exposed to the OpenCode operator.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Call runs the tool; the returned value is marshaled into the MCP text
	// content. Returning an error marks the tool result as an error.
	Call func(c *Client, args json.RawMessage) (any, error)
}

// obj builds a JSON-schema object type with optional required fields. A nil
// props still emits an empty object (never null) — strict MCP clients validate
// that properties is present and an object.
func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	return map[string]any{"type": "object", "properties": props, "required": strSlice(required)}
}

func strSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return in
}

func strProp(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func numProp(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

// ToolList is every tool the bridge exposes, in operator-workflow order.
var ToolList = []Tool{
	{
		Name: "artex_status",
		Description: "ARTEX bridge status: pending LLM requests (the queue this operator must serve), their ages/claims, " +
			"and the active LLM profile. Call this first in a session.",
		InputSchema: obj(nil),
		Call: func(c *Client, _ json.RawMessage) (any, error) {
			return c.Do("GET", "/api/mcp/status", nil)
		},
	},
	{
		Name: "artex_llm_next",
		Description: "Fetch the OLDEST pending completion request and claim it (FIFO). The request carries system, full message " +
			"history, tool schemas and max_tokens — answer it AS THE MODEL (planner or worker persona the system prompt describes), " +
			"then deliver the assistant turn with artex_llm_submit. wait_seconds long-polls when the queue is empty " +
			"(0-120; use ~60 while idling between polls). Returns {\"request\": null} when nothing arrived.",
		InputSchema: obj(
			map[string]any{"wait_seconds": numProp("seconds to long-poll for a request (0-120, default 0)")},
		),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			var in struct {
				WaitSeconds float64 `json:"wait_seconds"`
			}
			if len(args) > 0 {
				_ = json.Unmarshal(args, &in)
			}
			if in.WaitSeconds < 0 {
				in.WaitSeconds = 0
			}
			if in.WaitSeconds > 120 {
				in.WaitSeconds = 120
			}
			return c.Do("POST", "/api/mcp/llm/next", map[string]any{"wait_seconds": in.WaitSeconds})
		},
	},
	{
		Name: "artex_llm_submit",
		Description: "Deliver the assistant turn for a claimed request. blocks is an ordered array of content blocks: " +
			"{\"type\":\"text\",\"text\":...}, {\"type\":\"thinking\",...}, and/or {\"type\":\"tool_use\",\"name\":\"<tool>\"," +
			"\"input\":{...}} (omit tool id; input is a JSON object — stringified JSON is tolerated). stop_reason derives " +
			"automatically (tool_use when any tool call is present, else end_turn). usage is optional real token counts; " +
			"the bridge estimates when omitted. Answer EVERY claimed request exactly once.",
		InputSchema: obj(
			map[string]any{
				"request_id":  strProp("the id from artex_llm_next"),
				"blocks":      map[string]any{"type": "array", "description": "assistant content blocks (text / thinking / tool_use) in order", "items": map[string]any{"type": "object"}},
				"stop_reason": strProp("optional explicit stop reason; derived when omitted"),
				"usage":       map[string]any{"type": "object", "description": "optional {input_tokens, output_tokens} real counts"},
			},
			"request_id", "blocks",
		),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			var in struct {
				RequestID  string          `json:"request_id"`
				StopReason string          `json:"stop_reason"`
				Blocks     json.RawMessage `json:"blocks"`
				Usage      json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, fmt.Errorf("bad arguments: %v", err)
			}
			body := map[string]any{"request_id": in.RequestID, "blocks": in.Blocks}
			if in.StopReason != "" {
				body["stop_reason"] = in.StopReason
			}
			if len(in.Usage) > 0 && string(in.Usage) != "null" {
				body["usage"] = in.Usage
			}
			return c.Do("POST", "/api/mcp/llm/submit", body)
		},
	},
	{
		Name:        "artex_tasks",
		Description: "List ARTEX tasks (same payload as the web UI's task list).",
		InputSchema: obj(map[string]any{
			"status": strProp("optional status filter"),
			"limit":  numProp("optional row limit"),
		}),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			return c.Do("GET", "/api/mcp/tasks"+queryString(args, "status", "limit"), nil)
		},
	},
	{
		Name:        "artex_task",
		Description: "One task's full state: status, goals, intents, sessions — including intents waiting for an operator reply.",
		InputSchema: obj(map[string]any{"id": strProp("task id")}, "id"),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			id, err := argString(args, "id")
			if err != nil {
				return nil, err
			}
			return c.Do("GET", "/api/mcp/task/"+id, nil)
		},
	},
	{
		Name: "artex_task_create",
		Description: "Create (and launch) a penetration-testing task. goal is the objective; description is a short title. " +
			"Optionally pin llm_profile_ids (from artex_status / System→LLM) and a timeout_seconds budget.",
		InputSchema: obj(
			map[string]any{
				"goal":        strProp("the task objective"),
				"description": strProp("short title (default 'Unnamed task')"),
				"name":        strProp("optional display name"),
				"llm_profile_ids": map[string]any{
					"type": "array", "items": map[string]any{"type": "number"},
					"description": "ordered LLM profile chain for this task (omit to use the global active profile)",
				},
				"timeout_seconds": numProp("optional wall-clock budget in seconds"),
			},
			"goal",
		),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			return c.Do("POST", "/api/mcp/task/create", json.RawMessage(args))
		},
	},
	{
		Name:        "artex_task_control",
		Description: "Pause or resume a task's autonomous execution.",
		InputSchema: obj(
			map[string]any{
				"id":     strProp("task id"),
				"action": strProp("\"pause\" or \"resume\""),
			},
			"id", "action",
		),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			id, err := argString(args, "id")
			if err != nil {
				return nil, err
			}
			return c.Do("POST", "/api/mcp/task/"+id+"/control", stripKeys(args, "id"))
		},
	},
	{
		Name: "artex_intent_control",
		Description: "Pause, resume or cancel one worker intent. cancel requires a reason; mode is soft (default) or hard.",
		InputSchema: obj(
			map[string]any{
				"task_id":    strProp("task id"),
				"intent_id":  strProp("intent id"),
				"action":     strProp("\"pause\" | \"resume\" | \"cancel\""),
				"reason":     strProp("required for cancel: why"),
				"mode":       strProp("cancel only: soft (default) | hard"),
			},
			"task_id", "intent_id", "action",
		),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			taskID, err := argString(args, "task_id")
			if err != nil {
				return nil, err
			}
			intentID, err := argString(args, "intent_id")
			if err != nil {
				return nil, err
			}
			return c.Do("POST", "/api/mcp/task/"+taskID+"/intent/"+intentID+"/control", stripKeys(args, "task_id", "intent_id"))
		},
	},
	{
		Name: "artex_worker_message",
		Description: "Reply to a worker's ask_operator question (or steer an intent). The task must be running; the intent " +
			"should be paused via the question's flow. message max 4000 chars.",
		InputSchema: obj(
			map[string]any{
				"task_id":   strProp("task id"),
				"intent_id": strProp("intent id"),
				"message":   strProp("the operator reply"),
			},
			"task_id", "intent_id", "message",
		),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			taskID, err := argString(args, "task_id")
			if err != nil {
				return nil, err
			}
			intentID, err := argString(args, "intent_id")
			if err != nil {
				return nil, err
			}
			return c.Do("POST", "/api/mcp/task/"+taskID+"/intent/"+intentID+"/message", stripKeys(args, "task_id", "intent_id"))
		},
	},
	{
		Name:        "artex_findings",
		Description: "List findings (optionally filtered by task_id / severity, mirroring the web UI's findings view).",
		InputSchema: obj(map[string]any{
			"task_id":  strProp("optional task id filter"),
			"severity": strProp("optional severity filter"),
		}),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			return c.Do("GET", "/api/mcp/findings"+queryString(args, "task_id", "severity", "status", "limit"), nil)
		},
	},
	{
		Name:        "artex_activity",
		Description: "Recent activity/events for a task (worker sessions, tool calls, planner rounds).",
		InputSchema: obj(map[string]any{
			"task_id": strProp("task id"),
			"limit":   numProp("optional row limit"),
		}, "task_id"),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			return c.Do("GET", "/api/mcp/activity"+queryString(args, "task_id", "limit", "before"), nil)
		},
	},
	{
		Name:        "artex_intercept_pending",
		Description: "List pending interception approvals (tool executions / target requests awaiting a human decision).",
		InputSchema: obj(nil),
		Call: func(c *Client, _ json.RawMessage) (any, error) {
			return c.Do("GET", "/api/mcp/intercept/pending", nil)
		},
	},
	{
		Name: "artex_intercept_decide",
		Description: "Approve or deny a pending interception request. The OpenCode operator acts as the human approver — " +
			"review the request's argv/target carefully before allowing.",
		InputSchema: obj(
			map[string]any{
				"id":       strProp("pending request id"),
				"decision": strProp("\"allowed\" or \"denied\""),
			},
			"id", "decision",
		),
		Call: func(c *Client, args json.RawMessage) (any, error) {
			id, err := argString(args, "id")
			if err != nil {
				return nil, err
			}
			return c.Do("POST", "/api/mcp/intercept/pending/"+id+"/decide", stripKeys(args, "id"))
		},
	},
}

// --- JSON-RPC over stdio ---

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the MCP stdio server until stdin closes. All logs go to stderr
// (stdout belongs to the protocol).
func Serve(c *Client, version string, in io.Reader, out io.Writer) error {
	rd := bufio.NewReaderSize(in, 1<<20)
	enc := json.NewEncoder(out)
	tools := ToolList
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		trim := strings.TrimSpace(string(line))
		if trim == "" {
			if err != nil {
				return err
			}
			continue
		}
		var msg rpcMessage
		if json.Unmarshal([]byte(trim), &msg) != nil {
			continue // not JSON-RPC; ignore
		}
		if msg.Method == "" || (len(msg.ID) == 0 || string(msg.ID) == "null") {
			// Notification (e.g. notifications/initialized): no reply.
			if err != nil {
				return err
			}
			continue
		}
		result, rpcErr := handle(c, version, tools, msg)
		resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
		if err != nil {
			return err
		}
	}
}

// handle answers one JSON-RPC request.
func handle(c *Client, version string, tools []Tool, msg rpcMessage) (any, *rpcError) {
	switch msg.Method {
	case "initialize":
		proto := "2025-06-18"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.ProtocolVersion != "" {
			proto = p.ProtocolVersion // echo the client's version
		}
		return map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "artex", "version": version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		out := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			out = append(out, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		return map[string]any{"tools": out}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tools/call params: " + err.Error()}
		}
		for _, t := range tools {
			if t.Name != p.Name {
				continue
			}
			res, err := t.Call(c, p.Arguments)
			if err != nil {
				// Tool-level failure: still a well-formed MCP tool result, with
				// isError so the operator model sees and can recover from it.
				return map[string]any{
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
					"isError": true,
				}, nil
			}
			text, _ := json.MarshalIndent(res, "", "  ")
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(text)}},
			}, nil
		}
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
}

// --- small argument helpers ---

func argString(args json.RawMessage, key string) (string, error) {
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return "", fmt.Errorf("argument %q must not be empty", key)
		}
		return t, nil
	case float64:
		return fmt.Sprintf("%.0f", t), nil
	default:
		return "", fmt.Errorf("argument %q must be a string", key)
	}
}

// stripKeys returns the args object minus the given keys, for bodies that
// pass the rest through verbatim.
func stripKeys(args json.RawMessage, keys ...string) json.RawMessage {
	var m map[string]any
	if len(args) == 0 || json.Unmarshal(args, &m) != nil {
		return json.RawMessage("{}")
	}
	for _, k := range keys {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// queryString turns selected string/number args into a URL query.
func queryString(args json.RawMessage, keys ...string) string {
	var m map[string]any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &m)
	}
	var sb strings.Builder
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		s := ""
		switch t := v.(type) {
		case string:
			s = t
		case float64:
			s = fmt.Sprintf("%.0f", t)
		default:
			continue
		}
		if s == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(k + "=" + urlQueryEscape(s))
	}
	if sb.Len() == 0 {
		return ""
	}
	return "?" + sb.String()
}

func urlQueryEscape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~':
			sb.WriteRune(r)
		default:
			for _, b := range []byte(string(r)) {
				fmt.Fprintf(&sb, "%%%02X", b)
			}
		}
	}
	return sb.String()
}

// Run is the process entrypoint for `artex mcp-bridge`.
func Run(baseURL, token, version string) error {
	log.SetFlags(0)
	log.SetPrefix("[artex-mcp-bridge] ")
	log.Printf("serving %d tools for ARTEX at %s", len(ToolList), baseURL)
	return Serve(NewClient(baseURL, token), version, os.Stdin, os.Stdout)
}
