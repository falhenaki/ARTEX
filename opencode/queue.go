package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Timeouts. Both are overridable via environment so a slow operator session
// (long thinking, big tool batches) is not cut off mid-answer.
var (
	// QueueTimeout is how long an UNCLAIMED request waits for an operator to
	// fetch it via artex_llm_next before the parked call fails.
	QueueTimeout = envSeconds("ARTEX_OPENCODE_QUEUE_TIMEOUT", 10*time.Minute)
	// AnswerTimeout is how long a CLAIMED request waits for artex_llm_submit
	// before the parked call fails. Claiming happens the moment artex_llm_next
	// hands the request out, so this bounds the operator's thinking + submit
	// round trip.
	AnswerTimeout = envSeconds("ARTEX_OPENCODE_ANSWER_TIMEOUT", 30*time.Minute)
)

// ErrNoAnswer is returned to the parked LLM call when no operator answered in
// time. It deliberately carries no HTTP status: llmpool treats it as a
// transport-level failure (failover-eligible), and the message names the
// remedy.
type timeoutErr struct {
	id      string
	waited  time.Duration
	claimed bool
}

func (e *timeoutErr) Error() string {
	what := "fetched by an MCP operator"
	if e.claimed {
		what = "answered by an MCP operator"
	}
	return fmt.Sprintf("opencode bridge: request %s was not %s within %s (is an OpenCode session polling artex_llm_next?)", e.id, what, e.waited)
}

// ErrUnknownRequest reports a submit for a request that is no longer pending:
// already answered, expired, or its calling task was stopped.
var ErrUnknownRequest = errors.New("opencode bridge: unknown, expired or already-answered request")

// entry is one parked completion inside the Registry.
type entry struct {
	payload   Payload
	createdAt time.Time
	claimedAt time.Time // zero until an operator fetches it
	deadline  time.Time // createdAt+QueueTimeout, then claimedAt+AnswerTimeout
	respCh    chan Submission
}

// Call is the provider's handle on one parked request: it waits for the
// operator's answer and can be cancelled (task stop) to pull the request out
// of the queue.
type Call struct {
	reg     *Registry
	e       *entry
	done    chan struct{}
	doneOne sync.Once
}

// Cancel pulls the request from the queue and releases waiters. Idempotent;
// safe to call from defer alongside Wait.
func (c *Call) Cancel() {
	c.doneOne.Do(func() { close(c.done) })
	c.reg.remove(c.e)
}

// ID returns the request id the operator will reference in artex_llm_submit.
func (c *Call) ID() string { return c.e.payload.ID }

// Wait blocks until the operator answers, ctx is done, or the applicable
// timeout expires.
func (c *Call) Wait(ctx context.Context) (Submission, error) {
	defer c.Cancel()
	for {
		d := c.reg.deadlineOf(c.e)
		if d <= 0 {
			d = time.Millisecond
		}
		timer := time.NewTimer(d)
		select {
		case s := <-c.e.respCh:
			timer.Stop()
			return s, nil
		case <-ctx.Done():
			timer.Stop()
			return Submission{}, ctx.Err()
		case <-c.done:
			timer.Stop()
			return Submission{}, context.Canceled
		case <-timer.C:
			// The deadline may have moved (a claim extends it); recheck once.
			claimedAt := c.reg.claimedOf(c.e)
			if !claimedAt.IsZero() && time.Now().Before(claimedAt.Add(AnswerTimeout)) {
				continue
			}
			was, claimed := QueueTimeout, false
			if !claimedAt.IsZero() {
				was, claimed = AnswerTimeout, true
			}
			return Submission{}, &timeoutErr{id: c.e.payload.ID, waited: was, claimed: claimed}
		}
	}
}

// Registry is the process-wide park-and-answer queue. Providers enqueue here;
// the MCP-facing HTTP handlers claim and submit. Safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	entries []*entry
	wake    chan struct{} // closed+rebuilt whenever entries appear
}

// Default is the singleton used by providers built from "opencode" profiles
// and by the server's MCP endpoints. One queue for the whole process: every
// opencode-format profile shares it, FIFO.
var Default = NewRegistry()

// NewRegistry builds an empty queue.
func NewRegistry() *Registry { return &Registry{wake: make(chan struct{})} }

// Enqueue parks a payload and returns the provider's call handle.
func (r *Registry) Enqueue(p Payload) *Call {
	if p.ID == "" {
		p.ID = newID()
	}
	p.CreatedAt = time.Now()
	e := &entry{
		payload:  p,
		createdAt: p.CreatedAt,
		deadline: p.CreatedAt.Add(QueueTimeout),
		respCh:   make(chan Submission, 1),
	}
	r.mu.Lock()
	r.entries = append(r.entries, e)
	close(r.wake)
	r.wake = make(chan struct{})
	r.mu.Unlock()
	return &Call{reg: r, e: e, done: make(chan struct{})}
}

// Next claims and returns the oldest pending payload, marking it claimed (and
// extending its deadline to claimedAt+AnswerTimeout). When none is pending it
// waits up to wait for one. Returns ok=false when the wait elapsed empty.
func (r *Registry) Next(ctx context.Context, wait time.Duration) (Payload, bool) {
	deadline := time.Now().Add(wait)
	for {
		r.mu.Lock()
		now := time.Now()
		for i, e := range r.entries {
			if !e.claimedAt.IsZero() {
				continue
			}
			if now.After(e.deadline) {
				r.entries = append(r.entries[:i], r.entries[i+1:]...)
				continue // expired unclaimed; drop silently
			}
			e.claimedAt = now
			e.deadline = now.Add(AnswerTimeout)
			p := e.payload
			r.mu.Unlock()
			return p, true
		}
		wake := r.wake
		r.mu.Unlock()

		remain := time.Until(deadline)
		if remain <= 0 {
			return Payload{}, false
		}
		timer := time.NewTimer(remain)
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			return Payload{}, false
		case <-ctx.Done():
			timer.Stop()
			return Payload{}, false
		}
	}
}

// Submit delivers the operator's answer to a claimed request. Unknown,
// expired, answered or never-claimed ids return an error.
func (r *Registry) Submit(id string, s Submission) error {
	r.mu.Lock()
	var e *entry
	for _, x := range r.entries {
		if x.payload.ID == id {
			e = x
			break
		}
	}
	if e == nil {
		r.mu.Unlock()
		return ErrUnknownRequest
	}
	claimed := !e.claimedAt.IsZero()
	r.mu.Unlock()
	if !claimed {
		return fmt.Errorf("opencode bridge: request %s was never claimed; call artex_llm_next first", id)
	}
	select {
	case e.respCh <- s:
		r.remove(e)
		return nil
	default:
		return ErrUnknownRequest
	}
}

// deadlineOf returns the time until the entry's current deadline (<=0 when
// already past).
func (r *Registry) deadlineOf(e *entry) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Until(e.deadline)
}

// claimedOf reads an entry's claim time (zero when absent or unclaimed).
func (r *Registry) claimedOf(e *entry) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.claimedAt
}

// remove drops an entry (idempotent).
func (r *Registry) remove(e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, x := range r.entries {
		if x == e {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			return
		}
	}
}

// PendingStatus is one parked request as shown to the operator.
type PendingStatus struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	Claimed     bool      `json:"claimed"`
	SessionID   string    `json:"session_id,omitempty"`
	WaitSeconds int       `json:"wait_seconds"`
}

// Status describes the queue for the status endpoint.
type Status struct {
	Pending       []PendingStatus `json:"pending"`
	QueueTimeout  string          `json:"queue_timeout"`
	AnswerTimeout string          `json:"answer_timeout"`
}

// Snapshot copies the queue's current state (expired entries swept).
func (r *Registry) Snapshot() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := Status{
		Pending:       []PendingStatus{},
		QueueTimeout:  QueueTimeout.String(),
		AnswerTimeout: AnswerTimeout.String(),
	}
	kept := r.entries[:0]
	for _, e := range r.entries {
		if now.After(e.deadline) {
			continue // expired; the parked call's own timer reports the error
		}
		kept = append(kept, e)
		out.Pending = append(out.Pending, PendingStatus{
			ID: e.payload.ID, CreatedAt: e.payload.CreatedAt,
			Claimed: !e.claimedAt.IsZero(), SessionID: e.payload.SessionID,
			WaitSeconds: int(now.Sub(e.payload.CreatedAt).Seconds()),
		})
	}
	r.entries = kept
	return out
}

// newID mints a short unique request id ("oc-" + 8 hex chars).
func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "oc-" + hex.EncodeToString(b[:])
}

// envSeconds reads a positive-seconds environment variable, falling back to d.
func envSeconds(name string, d time.Duration) time.Duration {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return d
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return d
}
