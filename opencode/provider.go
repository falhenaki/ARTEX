package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// Provider implements llm.Provider by parking every completion in the
// Registry and waiting for an MCP operator (an OpenCode session) to answer.
// It performs no network I/O whatsoever.
type Provider struct {
	model string
	reg   *Registry
}

// NewProvider builds a bridge provider. model is display-only (UI, logs);
// the actual "model" is whoever answers artex_llm_next.
func NewProvider(model string) *Provider {
	return &Provider{model: model, reg: Default}
}

// Model returns the configured display model name.
func (p *Provider) Model() string { return p.model }

// Complete parks the request and returns the operator's answer as one
// assembled assistant message.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	sub, usage, err := p.ask(ctx, req)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	msg := llm.Message{Role: llm.RoleAssistant, Content: sub.Blocks}
	return msg, sub.DeriveStopReason(), usage, nil
}

// Stream parks the request and, once answered, replays it as the normalized
// event sequence a real streaming API would have produced. The harness's
// Accumulator rebuilds the exact message from these events.
func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		sub, usage, err := p.ask(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !emitEvents(yield, sub, usage) {
			return
		}
	}
}

// ask is the shared park-and-wait core.
func (p *Provider) ask(ctx context.Context, req llm.CompletionRequest) (Submission, llm.Usage, error) {
	payload := Payload{
		System:    req.System,
		Messages:  req.Messages,
		Tools:     req.Tools,
		MaxTokens: req.MaxTokens,
		Thinking:  req.Thinking,
		SessionID: transcript.SessionIDFrom(ctx),
	}
	payload.InputEstTokens = estimateTokens(req)
	call := p.reg.Enqueue(payload)
	sub, err := call.Wait(ctx)
	if err != nil {
		return Submission{}, llm.Usage{}, err
	}
	usage := sub.Usage
	if usage == nil {
		u := llm.Usage{
			InputTokens:  payload.InputEstTokens,
			OutputTokens: estimateBlocksTokens(sub.Blocks),
		}
		usage = &u
	}
	return sub, *usage, nil
}

// emitEvents synthesizes the event stream for a submission. Order matches
// what the adapters produce: message_start → thinking → text → tool_use
// blocks → message_delta (stop reason + usage) → message_stop.
func emitEvents(yield func(llm.StreamEvent, error) bool, sub Submission, usage llm.Usage) bool {
	if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: usage.InputTokens}}, nil) {
		return false
	}
	for i, b := range sub.Blocks {
		switch b.Type {
		case llm.BlockText:
			if b.Text != "" && !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
				return false
			}
		case llm.BlockThinking:
			if b.Thinking != "" && !yield(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: b.Thinking}, nil) {
				return false
			}
			if b.Signature != "" && !yield(llm.StreamEvent{Type: llm.SEThinkingSignature, Text: b.Signature}, nil) {
				return false
			}
		case llm.BlockToolUse:
			if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name, ToolIndex: i}, nil) {
				return false
			}
			input := string(b.Input)
			if input == "" {
				input = "{}"
			}
			if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: input, ToolIndex: i}, nil) {
				return false
			}
		}
	}
	if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: sub.DeriveStopReason(), Usage: usage}, nil) {
		return false
	}
	return yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
}

// ValidateSubmission normalizes an operator submission before it is delivered:
// synthesizes missing tool_use IDs, guarantees tool inputs are JSON objects,
// and rejects blocks a model turn can never contain (tool_result comes only
// from the harness; redacted thinking is provider-internal).
func ValidateSubmission(sub *Submission) error {
	for i := range sub.Blocks {
		b := &sub.Blocks[i]
		switch b.Type {
		case llm.BlockText, llm.BlockThinking:
			// nothing to normalize
		case llm.BlockToolUse:
			if b.Name == "" {
				return fmt.Errorf("block %d: tool_use without a name", i)
			}
			if b.ID == "" {
				b.ID = fmt.Sprintf("toolu_oc_%d_%d", i, time.Now().UnixNano())
			}
			raw := []byte(b.Input)
			if len(raw) == 0 {
				b.Input = json.RawMessage("{}")
				continue
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("block %d (tool %q): input is not valid JSON: %v", i, b.Name, err)
			}
			if _, ok := v.(map[string]any); !ok {
				return fmt.Errorf("block %d (tool %q): input must be a JSON object", i, b.Name)
			}
		default:
			return fmt.Errorf("block %d: type %q is not allowed in a submitted model turn", i, b.Type)
		}
	}
	return nil
}

// estimateTokens sizes a request in tokens (~3.5 chars/token), mirroring
// llmpool's deliberately crude estimator: it only needs to keep usage stats
// and budgets in the right ballpark when the operator reports no real counts.
func estimateTokens(req llm.CompletionRequest) int {
	n := 0
	for _, s := range req.System {
		n += len(s)
	}
	for _, m := range req.Messages {
		n += blocksLen(m.Content)
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)*120
	}
	return n * 2 / 7
}

// blocksLen sums the serialized size of a content-block tree (tool_result
// blocks nest their payload), matching llmpool's estimator.
func blocksLen(bs []llm.ContentBlock) int {
	n := 0
	for _, b := range bs {
		n += len(b.Text) + len(b.Thinking) + len(b.Input) + len(b.Name) + len(b.Data)
		if len(b.Content) > 0 {
			n += blocksLen(b.Content)
		}
	}
	return n
}

// estimateBlocksTokens sizes the operator's answer in tokens.
func estimateBlocksTokens(blocks []llm.ContentBlock) int {
	n := 0
	for _, b := range blocks {
		n += len(b.Text) + len(b.Thinking) + len(b.Input) + len(b.Name)
	}
	return n * 2 / 7
}
