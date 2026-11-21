// Package opencode implements the "opencode" LLM profile format: a keyless
// provider that makes NO outbound API calls. Instead of talking to an
// Anthropic/OpenAI endpoint, every completion is parked in a process-wide
// queue and answered by an external operator agent — an OpenCode session
// (GLM via the z.ai subscription) driving ARTEX over MCP.
//
// The flow:
//
//	planner/worker/chat → Provider.Stream/Complete → Registry.Enqueue
//	→ MCP tool artex_llm_next hands the request to the OpenCode agent
//	→ the agent produces the assistant turn
//	→ MCP tool artex_llm_submit → Registry.Submit → the parked call returns
//
// Token spend therefore happens inside the OpenCode subscription, never
// through an ARTEX-held API key.
package opencode

import (
	"time"

	"github.com/Autumn-27/norma/llm"
)

// Payload is one parked completion request as handed to the operator agent.
// Messages/Tools use norma's llm types directly: they marshal cleanly to JSON
// and the operator agent (itself an LLM) reads them as-is.
type Payload struct {
	ID        string         `json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	System    []string       `json:"system,omitempty"`
	Messages  []llm.Message  `json:"messages"`
	Tools     []llm.ToolSchema `json:"tools,omitempty"`
	MaxTokens int            `json:"max_tokens,omitempty"`
	// Thinking carries the per-request thinking override (""/"enabled"/"disabled"),
	// the same field norma would have put on the wire.
	Thinking string `json:"thinking,omitempty"`
	// SessionID identifies the ARTEX caller ("exp3-worker-i2", "exp3-planner",
	// "con-12", …) so the operator knows which role is asking.
	SessionID string `json:"session_id,omitempty"`
	// InputEstTokens is the bridge's own token estimate of the request, used for
	// usage accounting when the operator does not report real counts.
	InputEstTokens int `json:"input_est_tokens"`
}

// Submission is the operator's answer to a Payload: the next assistant turn.
type Submission struct {
	// StopReason mirrors the provider stop reason. Empty is derived from the
	// blocks: "tool_use" when any tool_use block is present, else "end_turn".
	StopReason string `json:"stop_reason,omitempty"`
	// Blocks is the assistant message content: text, thinking and tool_use
	// blocks in order. tool_use blocks need Name and Input (a JSON object);
	// ID is synthesized when omitted.
	Blocks []llm.ContentBlock `json:"blocks"`
	// Usage is optional real token accounting. When absent the bridge estimates
	// from payload size (~3.5 chars/token) so budgets and stats keep working.
	Usage *llm.Usage `json:"usage,omitempty"`
}

// DeriveStopReason fills the derived stop reason when the submitter sent none.
func (s Submission) DeriveStopReason() string {
	if s.StopReason != "" {
		return s.StopReason
	}
	for _, b := range s.Blocks {
		if b.Type == llm.BlockToolUse {
			return "tool_use"
		}
	}
	return "end_turn"
}
