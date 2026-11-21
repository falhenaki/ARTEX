# Running ARTEX through OpenCode (the MCP bridge)

ARTEX can run **without any LLM API key**: every planner, worker and chat
completion is answered by an OpenCode session — the same subscription that
runs your coding agent — and the whole platform (tasks, approvals, findings)
is operable over MCP. ARTEX makes **no outbound LLM API calls** in this mode.

```
planner / worker / chat ──▶ opencode bridge queue ──▶ artex_llm_next (MCP)
                                   ▲                            │
                                   └── artex_llm_submit ◀────────┘
                                     (the OpenCode agent IS the model)
```

## Pieces

| Piece | What it is |
| --- | --- |
| `opencode` LLM profile format | A keyless, endpoint-less profile. Its provider parks each completion in a process-wide queue and waits. Create it under **System → LLM** (format *OpenCode*) or `POST /api/llm/profiles` with `format: "opencode"`. |
| `/api/mcp/*` endpoints | Token-authenticated bridge + operations surface on the ARTEX server (outside the normal JWT auth). |
| `artex mcp-bridge` | An MCP **stdio server** subcommand that proxies MCP tool calls to those endpoints. Register it with OpenCode as a local MCP server. |

## Setup

1. **Token.** Set `ARTEX_MCP_TOKEN` on the ARTEX backend (docker-compose
   passes it through). If unset, ARTEX generates one per start and logs it:
   `docker compose logs artex | grep "MCP bridge token"`. Pinning it in `.env`
   is what you want in practice — the OpenCode config references it.

2. **Bridge binary on the host.** The subcommand lives in the normal binary:

   ```bash
   ./build.sh          # or: go build -o artex ./cmd/artex
   ```

   (Docker users can equally point the OpenCode command at
   `docker compose exec -T artex /app/artex mcp-bridge …` — anything that
   runs the subcommand with stdio works.)

3. **Register with OpenCode** — add to `~/.config/opencode/opencode.json`:

   ```json
   "mcp": {
     "artex": {
       "type": "local",
       "command": ["/Users/fawaz/Developer/ARTEX/artex", "mcp-bridge"],
       "enabled": true,
       "environment": {
         "ARTEX_URL": "http://127.0.0.1:8787",
         "ARTEX_MCP_TOKEN": "<your token>"
       }
     }
   }
   ```

4. **Create + activate the opencode profile** in the ARTEX UI (**System → LLM**,
   format *OpenCode*, then *Set as active*), or via the API. No key, no URL.

## Running

In the OpenCode session, the driving loop is:

1. `artex_status` — see the queue + active profile.
2. `artex_task_create` — start a task (or use the web UI).
3. `artex_llm_next` (long-poll `wait_seconds` ≈ 60) — receive a completion
   request: system prompt, full history, tool schemas.
4. Answer **as the model** (follow the system prompt's persona — planner or
   worker), then `artex_llm_submit` with your blocks: text and/or `tool_use`
   calls into ARTEX's own toolset. ARTEX executes the tools and loops.
5. Between polls: `artex_intercept_pending` / `artex_intercept_decide` for
   interception approvals, `artex_task` / `artex_activity` to watch progress,
   `artex_worker_message` to answer worker questions, `artex_findings` at the
   end.

One operator can serve several concurrent workers: requests are FIFO, and
each waits independently (`ARTEX_OPENCODE_QUEUE_TIMEOUT` unclaimed, default
600 s; `ARTEX_OPENCODE_ANSWER_TIMEOUT` once claimed, default 1800 s).

## Tool reference

| Tool | Purpose |
| --- | --- |
| `artex_status` | Bridge queue state + active LLM profile |
| `artex_llm_next` `{wait_seconds}` | Claim the oldest pending completion (long-poll) |
| `artex_llm_submit` `{request_id, blocks, stop_reason?, usage?}` | Deliver the assistant turn |
| `artex_tasks` `{status?, limit?}` | List tasks |
| `artex_task` `{id}` | Task detail (goals, intents, sessions) |
| `artex_task_create` `{goal, description?, …}` | Create + launch a task |
| `artex_task_control` `{id, action}` | `pause` / `resume` |
| `artex_intent_control` `{task_id, intent_id, action, …}` | pause / resume / cancel an intent |
| `artex_worker_message` `{task_id, intent_id, message}` | Reply to a worker question |
| `artex_findings` `{task_id?, severity?}` | Findings list |
| `artex_activity` `{task_id, limit?}` | Recent task activity |
| `artex_intercept_pending` | Pending interception approvals |
| `artex_intercept_decide` `{id, decision}` | Allow / deny (recorded as `mcp-operator`) |

## Notes & limits

- **Attribution.** Decisions taken over MCP are recorded in the audit trail as
  user `mcp-operator`. You are moving the human-in-the-loop seat to the
  OpenCode session — treat `artex_intercept_decide` with the same care a human
  approver would.
- **Usage accounting.** If the operator reports real token counts in
  `artex_llm_submit`, budgets and stats use them; otherwise the bridge
  estimates (~3.5 chars/token) so token budgets keep working.
- **Failover.** The opencode profile can sit in an LLM chain like any other;
  an unanswered request times out (no HTTP status in the error) and is treated
  as transport-level, i.e. failover-eligible.
- **Run budgets.** Worker and conversation wall-clock budgets count operator
  wait time (the harness enforces an absolute deadline it cannot pause), so
  when the resolved chain is opencode-backed ARTEX automatically inflates the
  per-run budget **4×** (logged at task build: `wall-clock budget 1200s →
  4800s`). Turn and token budgets are untouched — they measure work, which the
  bridge does not change. A task's own **timeout_seconds stays yours**: set it
  generously (or 0) for bridge-run tasks, because it is absolute wall clock
  from the first run and is not scaled.
- **Payload cap.** A `artex_llm_next` response is capped at ~2 MB: beyond it,
  the oldest tool_result payloads are replaced with a truncation marker.
