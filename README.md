# GoAgent v0.4 "GoMesh Reliability"

GoAgent is an open-source, universal agent template built in pure Go with zero external dependencies. It combines Hermes-grade log intelligence, self-learning pattern clustering, multi-tier security guardrails, and the **GoMesh** distributed leader/worker orchestration protocol.

GoAgent operates in three modes:
1. **Solo (default)**: Autonomous local agent connected to Telegram or CLI.
2. **Leader**: Cluster coordinator delegating subtasks across distributed workers and synthesizing results.
3. **Worker**: Specialized node executing assigned tasks with local tools and reporting back to the leader.

---

## Mesh Mode (GoMesh/1.1)

GoMesh is an HTTP/1.1 JSON-RPC 2.0 orchestration protocol with constant-time token authentication (`X-Mesh-Token`), capability-based routing, long-poll task delivery, and persistent JSONL state replay.

```
 [TG User]
     │ (Telegram Bot API)
     ▼
+─────────────────────────────────────────────────────────────+
│                    GoAgent Leader (:7801)                   │
+──────────────────────────────┬──────────────────────────────+
                               │
            ┌──────────────────┴──────────────────┐
            │ long-poll: GET /nexttask?id=...     │
            │ report & heartbeat: POST /rpc       │
            ▼                                     ▼
+───────────────────────+             +───────────────────────+
│   Worker A (logs)     │             │   Worker B (shell)    │
│ Capabilities: [logs]  │             │ Capabilities: [shell] │
+───────────────────────+             +───────────────────────+
```

### Quickstart: 3-Terminal Cluster (Inline Env)

Run a complete distributed GoMesh cluster on your local machine across 3 terminals:

**Terminal 1 (Leader / Coordinator):**
```bash
GOAGENT_ROLE=leader GOAGENT_MESH_LISTEN=:7801 GOAGENT_MESH_TOKEN=mesh-secret-123 LLM_API_KEY=your-api-key ./bin/goagent
```

**Terminal 2 (Worker A - Log Specialist):**
```bash
GOAGENT_ROLE=worker GOAGENT_LEADER_ADDR=http://127.0.0.1:7801 GOAGENT_MESH_TOKEN=mesh-secret-123 GOAGENT_WORKER_ID=worker-a GOAGENT_CAPABILITIES=logs LLM_API_KEY=your-api-key ./bin/goagent
```

**Terminal 3 (Worker B - Shell & Diagnostic Specialist):**
```bash
GOAGENT_ROLE=worker GOAGENT_LEADER_ADDR=http://127.0.0.1:7801 GOAGENT_MESH_TOKEN=mesh-secret-123 GOAGENT_WORKER_ID=worker-b GOAGENT_CAPABILITIES=shell LLM_API_KEY=your-api-key ./bin/goagent
```

### Protocol Summary
- **Auth**: Constant-time token comparison via `X-Mesh-Token`, protocol version via `X-Mesh-Proto: 1` (`GoMesh/1.1`).
- **Leader RPC (`POST /rpc`)**:
  - `mesh.register`: Registers worker ID and capabilities (`heartbeat_sec: 30`, poll endpoint `/nexttask`). Re-registering workers may include `known_tasks` (IDs they still execute); the response's `lost_tasks` lists those the leader no longer tracks as theirs — abort them locally (leader-restart duplicate-work guard).
  - `mesh.heartbeat`: Worker liveness heartbeat; returns pending task count and `abort_tasks` list (leader-ordered aborts for timed-out or cancelled work).
  - `mesh.add_mission`: Enqueue a mission directly via JSON-RPC with 1..50 subtasks.
  - `mesh.report`: Worker reports subtask completion or failure with execution metrics. Outputs are automatically sanitized by guardrails before transmission.
  - `mesh.get_mission_result`: Returns a mission's persisted final answer (`final_result`) and status — the recovery path after a leader restart.
- **Task Dequeue (`GET /nexttask?id=<worker_id>`)**:
  - Long-poll holding up to 50 seconds.
  - Returns HTTP 200 with task payload when matching task arrives.
  - Returns HTTP 204 No Content on poll timeout.
- **Liveness & Requeue**:
  - Leader sweeps workers every 15 seconds. If a worker misses 3 heartbeats (>90 seconds), it is marked dead and its in-flight tasks are automatically requeued.
  - In-flight tasks exceeding deadline are swept, aborted, and retried or failed.
  - Unfinished missions exceeding `GOAGENT_MISSION_TIMEOUT` (default 1800s) are safely cancelled.
- **Crash Recovery**: All mission and task transitions append to `data/mesh/state.jsonl` (event-sourcing, append-only, synchronous write before ack) and replay automatically on reboot. Any task that was in-flight when the leader died is restored to `pending` and re-dispatched — no task is lost.

### Reliability

GoMesh 1.1 includes multi-tier reliability mechanisms for resilient distributed execution:

- **Task Deadlines & Requeuing**: Every in-flight task is tracked with `AssignedAt`. If execution exceeds `GOAGENT_TASK_TIMEOUT` (or per-subtask `timeout_sec`), the leader sweeps the task from in-flight, notifies the worker to abort, and requeues it if attempts remain (`Attempts < GOAGENT_SUBTASK_ATTEMPTS`).
- **Abort Protocol via Heartbeat**: Leader returns `abort_tasks` in `mesh.heartbeat` responses when tasks time out or missions are cancelled. Workers maintain local execution cancel functions and abort running routines immediately, reporting `aborted by leader` to synchronize cluster state.
- **Subtask Retries**: Transient failures (network drops, LLM resets) automatically trigger subtask retries up to `GOAGENT_SUBTASK_ATTEMPTS` (default 2). Non-retryable errors (`aborted`, `no RunFunc`) fail immediately.
- **Auto-Fanout**: If a worker hits iteration budget exhaustion (`iteration budget`, `max iterations`), it invokes goal decomposition planning to split the workload into up to `GOAGENT_FANOUT_MAX` independent parallel sub-agents with divided deadlines. Recursion guards ensure sub-runs cannot trigger secondary fanouts.
- **Leader-Restart Claim Check**: When a worker re-registers (leader was down/restarted), it sends `known_tasks` — the task IDs it is still executing. The leader answers with `lost_tasks`: claimed tasks its replay no longer tracks as that worker's in-flight work (already requeued elsewhere). The worker aborts those locally, eliminating duplicate runs after a leader restart.
- **Mission Result Durability**: The leader's final synthesized answer is persisted onto the mission via the `set_mission_result` tool (and `mission_result` events in the log). A leader crash between synthesis and delivery no longer loses the answer — `mesh.get_mission_result` returns it after restart.

---

## Configuration Reference

Settings are loaded from `goagent.env` or system environment variables:

| Variable | Default | Mode | Description |
| :--- | :--- | :--- | :--- |
| `GOAGENT_ROLE` | `solo` | All | Operating mode: `solo`, `leader`, or `worker`. |
| `GOAGENT_MESH_LISTEN` | `:7801` | Leader | TCP address the leader HTTP server listens on. |
| `GOAGENT_MESH_TOKEN` | *(empty)* | Leader/Worker | Shared authentication token (mandatory for leader and worker). |
| `GOAGENT_LEADER_ADDR` | *(empty)* | Worker | Leader base URL (e.g. `http://192.168.1.10:7801`). |
| `GOAGENT_WORKER_ID` | `hostname-pid`| Worker | Unique identifier for the worker node. |
| `GOAGENT_CAPABILITIES`| *(empty)* | Worker | Comma-separated capability tags (e.g. `logs,files,shell`). |
| `GOAGENT_TASK_TIMEOUT`| `300s` | Leader | Maximum time permitted for a single subtask execution. |
| `GOAGENT_MISSION_TIMEOUT`| `1800s` | Leader | Maximum total lifecycle for a distributed mission. |
| `GOAGENT_SUBTASK_ATTEMPTS`| `2` | Leader | Maximum execution attempts per subtask before marking failed. |
| `GOAGENT_FANOUT_MAX` | `3` | Worker | Maximum parallel sub-agents spawned on iteration budget exhaustion (0 = off). |
| `TELEGRAM_TOKEN` | *(empty)* | Solo / Dual | Telegram Bot API token. Required in solo mode; optional in leader/worker. |
| `LLM_BASE_URL` | `https://api.openai.com/v1` | All | OpenAI-compatible endpoint URL. |
| `LLM_API_KEY` | *(empty)* | All | API key for the LLM provider. |
| `LLM_MODEL` | `gpt-4o-mini` | All | Primary model used for reasoning and completions. |
| `LLM_HEAVY_MODEL` | *(empty)* | All | Escalation model for complex turns (retry with half budget on failure). |
| `LLM_EMBED_URL` | *(empty)* | All | Custom OpenAI-compatible embedding endpoint URL. |
| `LLM_EMBED_MODEL` | *(empty)* | All | Model name for vector embeddings in knowledge base RAG. |
| `GOAGENT_ADMIN_CHAT_ID`| `0` | All | Restrict Telegram responses to a specific chat ID. |
| `GOAGENT_ALLOW_SHELL` | `false` | All | Enable arbitrary command execution (destructive commands still blocked). |
| `GOAGENT_MAX_ITER` | `8` | All | Maximum agent tool loop iterations per turn. |
| `GOAGENT_MASK_IP` | `false` | All | Mask IPv4 addresses (`x.x.x.<last-octet>`) in outputs. |
| `GOAGENT_ALLOWED_ROOTS`| `.` | All | Colon-separated filesystem boundaries for file tools. |
| `GOAGENT_ALLOW_ENV` | `false` | All | Permit reading `.env` files. |

---

## Built-in Tools

GoAgent includes built-in tools implemented strictly with the Go standard library:

### Universal & Diagnostic Tools
| Tool | Description |
| :--- | :--- |
| `analyze_logs` | Chunk-parallel log analyzer. Template clustering, burst detection, event correlation, and baseline deltas. |
| `log_timeline` | Extracts surrounding lines around a specific match or line number with secret redaction. |
| `read_log` | Reads the tail of a log file with optional regex filtering. |
| `learned_patterns` | Inspects historically learned pattern templates. |
| `read_file` | Reads paginated file contents within authorized roots. |
| `write_file` | Writes text content, creating parent directories atomically. |
| `list_dir` | Lists directories with file metadata and permissions. |
| `run_command` | Executes diagnostic shell commands with allowlist protection. |
| `fetch_url` | Fetches public HTTP(S) URLs, strips markup, and applies SSRF protection against loopback/private IPs. |
| `read_document` | Extracts text from `.docx`, `.xlsx`, `.pdf`, `.txt`, `.md`, `.csv`, `.json`, `.log` files without external libraries. |
| `save_knowledge` | Stores reference documents into the local RAG knowledge base. |
| `search_knowledge` | Performs cosine similarity top-k search with a 0.35 similarity floor over embedded documents. |
| `generate_pdf` | Generates formatted A4 PDF reports with page numbers and Latin-1 typography. CJK characters are guarded. |

### Leader Orchestration Tools (Role: Leader only)
| Tool | Description |
| :--- | :--- |
| `delegate_task` | Splits a high-level mission into subtasks with capability requirements. |
| `list_workers` | Lists connected workers, capabilities, in-flight work, and last seen timestamps. |
| `mission_status` | Returns complete status and state tree for a mission. |
| `cancel_mission` | Cancels pending and in-flight subtasks for a mission. |
| `collect_results` | Aggregates completed subtask outputs for leader synthesis. |
| `set_mission_result` | Persists the final synthesized answer for a mission to survive leader restarts. |
| `mesh_info` | Displays cluster configuration and active task counts. |

---

## Security & Guardrails

GoAgent enforces defense-in-depth security at every step:
- **Secret Redaction**: Automatic detection and redaction of API keys, private keys, passwords, database URLs, cards, and tokens before entering LLM context and before leaving the agent.
- **Untrusted Content Framing**: Content from `fetch_url` and `read_document` is wrapped with `FrameDocumentContent` (`· ` prefix per line) so embedded prompt injections are processed as DATA, never instructions.
- **SSRF Guard**: Custom HTTP dialer rejects loopback (`127.0.0.0/8`), private (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), link-local, multicast, and carrier-grade NAT addresses post-DNS.
- **Path & Command Guard**: Restricts file access to authorized roots and forbids destructive shell commands (`rm -rf`, `mkfs`, fork bombs, etc.).

---

## Extending GoAgent

1. **Adding a Tool**:
   Implement a handler (`func(argsJSON string) (string, error)`) and register it via `r.Register()` with `llm.ToolDefinition`:
   ```go
   r.Register(llm.ToolDefinition{
       Type: "function",
       Function: llm.ToolFunctionDef{
           Name: "my_tool",
           Description: "Tool description for the model",
           Parameters: map[string]any{ ... },
       },
   }, func(argsJSON string) (string, error) {
       // tool logic
       return "result", nil
   })
   ```
2. **Adding Capability Tags**:
   Tag worker nodes with arbitrary capability identifiers using `GOAGENT_CAPABILITIES=gpu,vision,storage,logs`. When the leader delegates a task requiring a capability (e.g. `capability: "gpu"`), GoMesh automatically routes it to workers matching that tag.
3. **Transport Adapters (Interface)**:
   GoMesh leader and worker decouple transport via stdlib HTTP interfaces. Future transport adapters (e.g. WebSocket duplex streaming, mTLS, libp2p, or Unix sockets) can be plugged in by wrapping `net.Listener` on the server and `http.RoundTripper` on the client.
4. **TODO: Deck / Slide Presentation Generator**:
   The `generate_pdf` tool provides pure-Go document rendering. A standalone slide deck generator (PPTX) can be added as an extension point using standard OOXML zip generation.

---

## License

MIT License. See [LICENSE](LICENSE) for details.
