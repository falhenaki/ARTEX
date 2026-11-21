package opencode

import (
	"context"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestCompleteRoundtrip(t *testing.T) {
	reg := NewRegistry()
	p := &Provider{model: "test", reg: reg}
	go func() {
		pl, ok := reg.Next(context.Background(), 5*time.Second)
		if !ok {
			t.Errorf("Next: nothing arrived")
			return
		}
		sub := Submission{Blocks: []llm.ContentBlock{
			llm.TextBlock("hello"),
			{Type: llm.BlockToolUse, Name: "bash", Input: []byte(`{"cmd":"id"}`)},
		}}
		if err := reg.Submit(pl.ID, sub); err != nil {
			t.Errorf("Submit: %v", err)
		}
	}()
	msg, stop, usage, err := p.Complete(context.Background(), llm.CompletionRequest{
		System:   []string{"sys"},
		Messages: []llm.Message{llm.UserText("run id")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if stop != "tool_use" {
		t.Fatalf("stop = %q, want tool_use", stop)
	}
	if msg.Text() != "hello" {
		t.Fatalf("text = %q", msg.Text())
	}
	tu := msg.ToolUses()
	if len(tu) != 1 || tu[0].Name != "bash" || string(tu[0].Input) != `{"cmd":"id"}` {
		t.Fatalf("tool uses = %+v", tu)
	}
	if usage.InputTokens == 0 || usage.OutputTokens == 0 {
		t.Fatalf("usage not estimated: %+v", usage)
	}
}

func TestStreamSynthesisRebuildsMessage(t *testing.T) {
	reg := NewRegistry()
	p := &Provider{model: "test", reg: reg}
	subCh := make(chan struct{})
	go func() {
		pl, ok := reg.Next(context.Background(), 5*time.Second)
		if !ok {
			t.Errorf("Next: nothing arrived")
			return
		}
		_ = reg.Submit(pl.ID, Submission{Blocks: []llm.ContentBlock{
			{Type: llm.BlockThinking, Thinking: "hmm", Signature: "sig1"},
			llm.TextBlock("answer"),
			{Type: llm.BlockToolUse, ID: "t1", Name: "webfetch", Input: []byte(`{"url":"https://x"}`)},
		}})
		close(subCh)
	}()

	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		acc.Add(ev)
	}
	<-subCh
	msg := acc.Message()
	if acc.StopReason != "tool_use" {
		t.Fatalf("stop = %q", acc.StopReason)
	}
	if len(msg.Content) != 3 {
		t.Fatalf("blocks = %d, want 3 (thinking, text, tool_use)", len(msg.Content))
	}
	if msg.Content[0].Type != llm.BlockThinking || msg.Content[0].Thinking != "hmm" || msg.Content[0].Signature != "sig1" {
		t.Fatalf("thinking block = %+v", msg.Content[0])
	}
	if msg.Content[1].Text != "answer" {
		t.Fatalf("text block = %+v", msg.Content[1])
	}
	tu := msg.Content[2]
	if tu.Type != llm.BlockToolUse || tu.ID != "t1" || tu.Name != "webfetch" || string(tu.Input) != `{"url":"https://x"}` {
		t.Fatalf("tool block = %+v", tu)
	}
}

func TestUnclaimedTimeout(t *testing.T) {
	oldQ, oldA := QueueTimeout, AnswerTimeout
	QueueTimeout, AnswerTimeout = 40*time.Millisecond, time.Minute
	defer func() { QueueTimeout, AnswerTimeout = oldQ, oldA }()

	reg := NewRegistry()
	call := reg.Enqueue(Payload{})
	_, err := call.Wait(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	te, ok := err.(*timeoutErr)
	if !ok {
		t.Fatalf("error type %T", err)
	}
	if te.claimed {
		t.Fatal("timeout should be the unclaimed flavour")
	}
}

func TestClaimedAnswerTimeout(t *testing.T) {
	oldQ, oldA := QueueTimeout, AnswerTimeout
	QueueTimeout, AnswerTimeout = time.Hour, 50*time.Millisecond
	defer func() { QueueTimeout, AnswerTimeout = oldQ, oldA }()

	reg := NewRegistry()
	call := reg.Enqueue(Payload{})
	if _, ok := reg.Next(context.Background(), time.Second); !ok {
		t.Fatal("Next did not claim")
	}
	_, err := call.Wait(context.Background())
	te, ok := err.(*timeoutErr)
	if !ok {
		t.Fatalf("error type %T", err)
	}
	if !te.claimed {
		t.Fatal("timeout should be the claimed flavour")
	}
}

func TestCancelPullsRequest(t *testing.T) {
	reg := NewRegistry()
	call := reg.Enqueue(Payload{})
	call.Cancel()
	if _, ok := reg.Next(context.Background(), 10*time.Millisecond); ok {
		t.Fatal("cancelled request still handed out")
	}
	if st := reg.Snapshot(); len(st.Pending) != 0 {
		t.Fatalf("snapshot after cancel = %+v", st)
	}
}

func TestSubmitUnknown(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Submit("nope", Submission{}); err == nil {
		t.Fatal("expected ErrUnknownRequest")
	}
}

func TestSubmitBeforeClaimRejected(t *testing.T) {
	reg := NewRegistry()
	call := reg.Enqueue(Payload{})
	if err := reg.Submit(call.e.payload.ID, Submission{}); err == nil {
		t.Fatal("submit before claim must fail")
	}
}

func TestValidateSubmission(t *testing.T) {
	sub := Submission{Blocks: []llm.ContentBlock{
		{Type: llm.BlockToolUse, Name: "bash", Input: []byte(`{"cmd":"id"}`)},
	}}
	if err := ValidateSubmission(&sub); err != nil {
		t.Fatalf("valid submission: %v", err)
	}
	if sub.Blocks[0].ID == "" {
		t.Fatal("tool id not synthesized")
	}

	bad := Submission{Blocks: []llm.ContentBlock{
		{Type: llm.BlockToolUse, Name: "bash", Input: []byte(`not json`)},
	}}
	if err := ValidateSubmission(&bad); err == nil {
		t.Fatal("invalid JSON input accepted")
	}

	wrongType := Submission{Blocks: []llm.ContentBlock{
		{Type: llm.BlockToolResult, ToolUseID: "x"},
	}}
	if err := ValidateSubmission(&wrongType); err == nil {
		t.Fatal("tool_result block accepted in a model turn")
	}

	arr := Submission{Blocks: []llm.ContentBlock{
		{Type: llm.BlockToolUse, Name: "bash", Input: []byte(`[1,2]`)},
	}}
	if err := ValidateSubmission(&arr); err == nil {
		t.Fatal("non-object tool input accepted")
	}
}

func TestFIFOOrder(t *testing.T) {
	reg := NewRegistry()
	c1 := reg.Enqueue(Payload{SessionID: "first"})
	reg.Enqueue(Payload{SessionID: "second"})
	p, ok := reg.Next(context.Background(), time.Second)
	if !ok || p.SessionID != "first" {
		t.Fatalf("Next = %+v ok=%v", p, ok)
	}
	p2, ok := reg.Next(context.Background(), time.Second)
	if !ok || p2.SessionID != "second" {
		t.Fatalf("Next2 = %+v ok=%v", p2, ok)
	}
	c1.Cancel()
	if st := reg.Snapshot(); len(st.Pending) != 1 || st.Pending[0].SessionID != "second" {
		t.Fatalf("snapshot = %+v", st.Pending)
	}
}

func TestDeriveStopReason(t *testing.T) {
	if s := (Submission{}).DeriveStopReason(); s != "end_turn" {
		t.Fatalf("empty = %q", s)
	}
	if s := (Submission{Blocks: []llm.ContentBlock{llm.TextBlock("x")}}).DeriveStopReason(); s != "end_turn" {
		t.Fatalf("text = %q", s)
	}
	if s := (Submission{Blocks: []llm.ContentBlock{{Type: llm.BlockToolUse, Name: "t"}}}).DeriveStopReason(); s != "tool_use" {
		t.Fatalf("tool = %q", s)
	}
	if s := (Submission{StopReason: "max_tokens"}).DeriveStopReason(); s != "max_tokens" {
		t.Fatalf("explicit = %q", s)
	}
}

func TestProviderBridgeBacked(t *testing.T) {
	p := NewProvider("m")
	if !p.BridgeBacked() {
		t.Fatal("bridge provider must report BridgeBacked=true")
	}
}
