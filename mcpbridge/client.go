// Package mcpbridge is the MCP stdio server that lets an OpenCode session run
// ARTEX end to end. Spawned as `artex mcp-bridge`, it speaks MCP over
// stdin/stdout (newline-delimited JSON-RPC) and proxies every tool call to the
// running ARTEX server's /api/mcp/* endpoints (bearer-token authenticated).
//
// The point of the bridge: every token ARTEX would have spent on an LLM API is
// instead served by the OpenCode operator itself — artex_llm_next hands the
// operator a parked completion request, the operator (an LLM inside the
// subscription) produces the assistant turn, artex_llm_submit delivers it.
// No API key ever exists on the ARTEX side.
package mcpbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the ARTEX /api/mcp/* endpoints.
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8787
	Token   string // ARTEX_MCP_TOKEN value
	HTTP    *http.Client
}

// NewClient builds a client. baseURL must not end with a slash.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 180 * time.Second},
	}
}

// apiError carries a non-2xx reply so tool results can surface the exact
// status + body instead of a collapsed transport error.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("artex api: HTTP %d: %s", e.Status, e.Body)
}

// Do performs one API call. body may be nil (GET). Returns the raw response
// body on success.
func (c *Client) Do(method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artex api unreachable at %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		return nil, &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	return json.RawMessage(raw), nil
}
