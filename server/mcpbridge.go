// MCP-facing endpoints for the OpenCode bridge. These live under /api/mcp/*
// OUTSIDE the normal JWT auth: they are authenticated by a dedicated bridge
// token (ARTEX_MCP_TOKEN, auto-generated and logged at startup when unset)
// because the caller is not a browser session but the `artex mcp-bridge`
// process spawned by an OpenCode session.
//
// Two groups:
//   - the LLM queue: artex_llm_next / artex_llm_submit talk to
//     opencode.Default directly (no dispatch);
//   - operations (tasks, findings, activity, worker messages, interception
//     approvals): thin internal dispatches onto the EXISTING handlers, so the
//     MCP path and the web UI share one source of truth for every action.
//     Dispatched requests carry a minted "mcp-operator" JWT so decision
//     audits record who acted.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/opencode"
	"github.com/Autumn-27/norma/llm"
)

// mcpPayloadCap bounds one artex_llm_next response (~2 MB). Beyond it the
// OLDEST tool_result payloads are replaced with a truncation marker: the
// operator still sees the conversation's shape, and compaction normally keeps
// requests well under this bound anyway.
const mcpPayloadCap = 2 << 20

// mcpMaxBodyBytes bounds every /api/mcp request body (submissions carry a full
// assistant turn; 8 MB is a generous ceiling).
const mcpMaxBodyBytes = 8 << 20

var (
	mcpTokenOnce sync.Once
	mcpTokenVal  string
)

// mcpBridgeToken returns the bridge token: ARTEX_MCP_TOKEN when set, else a
// random 24-hex-char token generated once per process (and logged, so a local
// operator can copy it from the server log).
func mcpBridgeToken() string {
	mcpTokenOnce.Do(func() {
		if t := strings.TrimSpace(os.Getenv("ARTEX_MCP_TOKEN")); t != "" {
			mcpTokenVal = t
			return
		}
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			log.Printf("[mcp-bridge] cannot generate token: %v; MCP endpoints disabled", err)
			mcpTokenVal = ""
			return
		}
		mcpTokenVal = hex.EncodeToString(b[:])
		log.Printf("[mcp-bridge] generated MCP bridge token (set ARTEX_MCP_TOKEN to pin it): %s", mcpTokenVal)
	})
	return mcpTokenVal
}

// requireMCPToken guards /api/mcp/*: bearer token, constant-time compare.
// 503 (not 401) when no token could be established, so a misconfigured
// deployment fails loudly instead of accepting everything.
func (s *Server) requireMCPToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := mcpBridgeToken()
		if tok == "" {
			writeErr(w, http.StatusServiceUnavailable, "MCP bridge token unavailable")
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid or missing MCP bridge token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, mcpMaxBodyBytes)
		next(w, r)
	}
}

// registerMCPRoutes mounts the bridge endpoints on the ROOT mux (before the
// JWT-wrapped /api/ subtree): exact patterns win over the /api/ prefix.
func (s *Server) registerMCPRoutes(root *http.ServeMux) {
	guard := func(h http.HandlerFunc) http.Handler { return s.requireMCPToken(h) }
	root.Handle("GET /api/mcp/status", guard(s.mcpStatus))
	root.Handle("POST /api/mcp/llm/next", guard(s.mcpLLMNext))
	root.Handle("POST /api/mcp/llm/submit", guard(s.mcpLLMSubmit))
	root.Handle("GET /api/mcp/tasks", guard(s.mcpTasks))
	root.Handle("POST /api/mcp/task/create", guard(s.mcpTaskCreate))
	root.Handle("GET /api/mcp/task/{id}", guard(s.mcpTaskGet))
	root.Handle("POST /api/mcp/task/{id}/control", guard(s.mcpTaskControl))
	root.Handle("POST /api/mcp/task/{id}/intent/{iid}/control", guard(s.mcpIntentControl))
	root.Handle("POST /api/mcp/task/{id}/intent/{iid}/message", guard(s.mcpWorkerMessage))
	root.Handle("GET /api/mcp/findings", guard(s.mcpFindings))
	root.Handle("GET /api/mcp/activity", guard(s.mcpActivity))
	root.Handle("GET /api/mcp/intercept/pending", guard(s.mcpInterceptPending))
	root.Handle("POST /api/mcp/intercept/pending/{id}/decide", guard(s.mcpInterceptDecide))
}

// --- LLM queue ---

// mcpStatus reports the bridge queue plus the active LLM profile, so the
// operator can see at a glance what is waiting and which profile the engine
// resolves to. Task state comes from artex_tasks / artex_task.
func (s *Server) mcpStatus(w http.ResponseWriter, r *http.Request) {
	st := opencode.Default.Snapshot()
	out := map[string]any{
		"bridge": st,
	}
	if s.m != nil && s.m.pg != nil {
		if p, err := s.m.pg.ActiveProfile(); err == nil && p != nil {
			out["active_profile"] = map[string]any{"id": p.ID, "name": p.Name, "format": p.Format, "model": p.Model}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// mcpLLMNext hands the oldest pending completion to the operator. wait_seconds
// enables long-polling (max 120) so an idle session parks instead of
// busy-polling. Returns {"request": null} when the wait elapsed empty.
func (s *Server) mcpLLMNext(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WaitSeconds float64 `json:"wait_seconds"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
			writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
			return
		}
	}
	wait := time.Duration(body.WaitSeconds * float64(time.Second))
	if wait < 0 {
		wait = 0
	}
	if wait > 120*time.Second {
		wait = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait+5*time.Second)
	defer cancel()
	p, ok := opencode.Default.Next(ctx, wait)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"request": nil})
		return
	}
	p = shrinkPayload(p, mcpPayloadCap)
	writeJSON(w, http.StatusOK, map[string]any{"request": p})
}

// mcpLLMSubmit delivers the operator's assistant turn to the parked call.
func (s *Server) mcpLLMSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID  string          `json:"request_id"`
		StopReason string          `json:"stop_reason"`
		Blocks     json.RawMessage `json:"blocks"`
		Usage      *llm.Usage      `json:"usage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.RequestID == "" {
		writeErr(w, http.StatusBadRequest, "request_id is required")
		return
	}
	if len(body.Blocks) == 0 {
		writeErr(w, http.StatusBadRequest, "blocks is required: an array of content blocks (text / thinking / tool_use)")
		return
	}
	var blocks []blockJSON
	if err := json.Unmarshal(body.Blocks, &blocks); err != nil {
		writeErr(w, http.StatusBadRequest, "blocks: "+err.Error())
		return
	}
	sub := opencode.Submission{StopReason: body.StopReason}
	for _, b := range blocks {
		cb, err := b.toContentBlock()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		sub.Blocks = append(sub.Blocks, cb)
	}
	sub.Usage = body.Usage
	if err := opencode.ValidateSubmission(&sub); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := opencode.Default.Submit(body.RequestID, sub); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "request_id": body.RequestID, "stop_reason": sub.DeriveStopReason()})
}

// blockJSON decodes one submitted content block tolerantly: tool_use input may
// arrive as a JSON object (preferred) or as a string containing JSON — the
// operator is an LLM writing JSON by hand, so both spellings are accepted.
type blockJSON struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

func (b *blockJSON) toContentBlock() (llm.ContentBlock, error) {
	switch b.Type {
	case "text":
		return llm.TextBlock(b.Text), nil
	case "thinking":
		return llm.ContentBlock{Type: llm.BlockThinking, Thinking: b.Thinking, Signature: b.Signature}, nil
	case "tool_use":
		raw := bytes.TrimSpace(b.Input)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			raw = []byte("{}")
		} else if raw[0] == '"' {
			// A stringified JSON object: unwrap it.
			var s string
			if json.Unmarshal(raw, &s) == nil {
				raw = []byte(strings.TrimSpace(s))
			}
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return llm.ContentBlock{}, fmt.Errorf("tool_use %q: input is not valid JSON", b.Name)
		}
		if _, ok := v.(map[string]any); !ok {
			return llm.ContentBlock{}, fmt.Errorf("tool_use %q: input must be a JSON object", b.Name)
		}
		return llm.ContentBlock{Type: llm.BlockToolUse, ID: b.ID, Name: b.Name, Input: raw}, nil
	default:
		return llm.ContentBlock{}, fmt.Errorf("block type %q is not allowed (use text, thinking or tool_use)", b.Type)
	}
}

// --- operations: internal dispatch onto the existing handlers ---

// captureWriter records a dispatched handler's response.
type captureWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) WriteHeader(status int)      { c.status = status }
func (c *captureWriter) Write(b []byte) (int, error) { return c.buf.Write(b) }

// dispatch runs one existing handler in-process with a fabricated request.
// The request carries a minted "mcp-operator" JWT (so audit trails attribute
// decisions) and the caller's query string / body verbatim. The handler's
// status+body are replayed onto w unchanged.
func (s *Server) dispatch(w http.ResponseWriter, method, path, rawQuery string, pathValues map[string]string, body []byte, h http.HandlerFunc) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = http.NoBody
	}
	req, err := http.NewRequestWithContext(context.Background(), method, path, rdr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "dispatch: "+err.Error())
		return
	}
	req.URL.RawQuery = rawQuery
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	// Attribute everything the dispatched handler does to a stable operator id.
	if tok, err := signJWTSubject(s.jwtKey, "mcp-operator"); err == nil {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	cw := &captureWriter{header: http.Header{}}
	h(cw, req)
	for k, vs := range cw.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if cw.status == 0 {
		cw.status = http.StatusOK
	}
	w.WriteHeader(cw.status)
	_, _ = w.Write(cw.buf.Bytes())
}

// passThrough reads the caller's body (nil for GET) for dispatch.
func passThrough(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	return b
}

func (s *Server) mcpTasks(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, http.MethodGet, "/api/tasks", r.URL.RawQuery, nil, nil, s.listTasks)
}

func (s *Server) mcpTaskCreate(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, http.MethodPost, "/api/tasks", "", nil, passThrough(r), s.createTask)
}

func (s *Server) mcpTaskGet(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, http.MethodGet, "/api/tasks/"+r.PathValue("id"), r.URL.RawQuery, map[string]string{"id": r.PathValue("id")}, nil, s.getTask)
}

func (s *Server) mcpTaskControl(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, http.MethodPost, "/api/tasks/"+r.PathValue("id")+"/control", "", map[string]string{"id": r.PathValue("id")}, passThrough(r), s.control)
}

func (s *Server) mcpIntentControl(w http.ResponseWriter, r *http.Request) {
	id, iid := r.PathValue("id"), r.PathValue("iid")
	s.dispatch(w, http.MethodPost, "/api/tasks/"+id+"/intents/"+iid+"/control", "", map[string]string{"id": id, "iid": iid}, passThrough(r), s.controlIntent)
}

func (s *Server) mcpWorkerMessage(w http.ResponseWriter, r *http.Request) {
	id, iid := r.PathValue("id"), r.PathValue("iid")
	s.dispatch(w, http.MethodPost, "/api/tasks/"+id+"/intents/"+iid+"/messages", "", map[string]string{"id": id, "iid": iid}, passThrough(r), s.sendWorkerMessage)
}

func (s *Server) mcpFindings(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, http.MethodGet, "/api/exploration/findings", r.URL.RawQuery, nil, nil, s.findings)
}

func (s *Server) mcpActivity(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, http.MethodGet, "/api/exploration/activity", r.URL.RawQuery, nil, nil, s.activity)
}

func (s *Server) mcpInterceptPending(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, http.MethodGet, "/api/intercept/pending", r.URL.RawQuery, nil, nil, s.interceptListPending)
}

func (s *Server) mcpInterceptDecide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.dispatch(w, http.MethodPost, "/api/intercept/pending/"+id+"/decide", "", map[string]string{"id": id}, passThrough(r), s.interceptDecide)
}

// --- payload hygiene ---

// shrinkPayload bounds a payload for the MCP transport: when the serialized
// size exceeds cap, the OLDEST tool_result text payloads are replaced with a
// marker (newest results are what the decision needs most). Structure is
// preserved; only oversized payloads are clipped.
func shrinkPayload(p opencode.Payload, cap int) opencode.Payload {
	b, err := json.Marshal(p)
	if err != nil || len(b) <= cap {
		return p
	}
	const marker = "[opencode-bridge: tool_result truncated for transport]"
	out := p
	for i := range out.Messages {
		for j := range out.Messages[i].Content {
			blk := &out.Messages[i].Content[j]
			if blk.Type != "tool_result" {
				continue
			}
			for k := range blk.Content {
				if blk.Content[k].Type == "text" && len(blk.Content[k].Text) > len(marker)*4 {
					blk.Content[k].Text = marker
					if b, err := json.Marshal(out); err == nil && len(b) <= cap {
						return out
					}
				}
			}
		}
	}
	return out
}
