# GoAgent v0.2

GoAgent is an open-source, general-purpose Telegram assistant built in pure Go that specializes in Hermes-grade log intelligence, self-learning pattern clustering, and system diagnostics with zero-leak guardrails. It connects OpenAI-compatible LLMs to your local environment using chunk-parallel log engines, concurrent multi-tool execution, and multi-tier security boundaries with zero external dependencies.

---

## Quick Start

Get your bot running in 5 easy steps:

```bash
# 1. Clone the repository
git clone https://github.com/metabbe3/GoAgent.git
cd GoAgent

# 2. Get your bot token from @BotFather on Telegram (https://t.me/BotFather)

# 3. Run the interactive installer (sets up env, builds binary, installs system service)
./deploy.sh install

# 4. Check the service status
./deploy.sh status

# 5. Open Telegram, send /start to your bot, and begin chatting!
```

---

## Tools

GoAgent equips the LLM with built-in tools for inspecting systems and analyzing logs:

| Tool | Parameters | Description |
| :--- | :--- | :--- |
| `analyze_logs` | `path`, `top?`, `filter_regex?`, `min_count?`, `compare_baseline?`, `window?` | High-speed chunk-parallel log analysis. Clusters logs into normalized templates, calculates severity distributions, autodetects formats, detects bursts, correlates sequential events, and flags anomalies. |
| `log_timeline` | `path`, `pattern_regex?`, `around_line?`, `context?` | Inspects surrounding context lines ($\pm N$, up to 50 lines max) around a specific line number or first pattern match, with automatic secret redaction. |
| `read_log` | `path`, `tail?`, `filter_regex?` | Reads the most recent lines of a log file with optional regex filtering. |
| `learned_patterns` | `file?` | Retrieves historical pattern memory and frequency statistics across past analyses. |
| `read_file` | `path`, `offset?`, `limit?` | Reads lines from a file with pagination support (guarded by allowed roots). |
| `write_file` | `path`, `content` | Writes text content to a file, automatically creating parent directories. |
| `list_dir` | `path?` | Lists directory contents with file size, permissions, and timestamps. |
| `run_command` | `cmd`, `timeout_sec?` | Executes diagnostic shell commands. Enforces permanent destructive command blocklist and safe diagnostic allowlist. |

---

## Log Intelligence v2

The v0.2 log engine brings Hermes-grade pattern intelligence to unstructured logs:

### 1. Format Autodetection
The engine automatically samples the first 100 lines to determine the log format without manual configuration:
- `json`: Lines parsed as structured JSON; extracts severity level (`level`, `lvl`, `severity`), timestamps (`ts`, `time`, `@timestamp`), and normalizes messages (`msg`, `message`, `error`).
- `logfmt`: Key-value pairs (`key=value`, `key="value"`); extracts level and normalizes dynamic values.
- `apache/nginx`: Combined Log Format; clusters HTTP request methods, paths, and status codes (5xx $\to$ ERROR, 4xx $\to$ WARN, 2xx/3xx $\to$ INFO).
- `syslog`: Standard RFC 3164/5424 syslog lines.
- `raw`: General unstructured log fallback.

### 2. Frequency Burst Detection (`burst.go`)
- Calculates a dynamic baseline across time windows (1-minute buckets, or 1-hour buckets for files spanning $>24$ hours).
- Computes mean and standard deviation per pattern across the timeline.
- Flags anomalous spikes where $\text{count} > \text{mean} + 3\sigma$ and $\text{count} \ge 5$, outputting top bursts ranked by $z$-score.

### 3. Sequential Event Correlation (`correlation.go`)
- Tracks consecutive lines across parsed timestamps.
- Identifies pairs of distinct patterns $(A \to B)$ that repeatedly occur within $\pm 2$ seconds of each other.
- Filters pairs with frequency $\ge 3$ and reports average time delta in milliseconds.

### 4. 30-Day Historical Baseline Delta (`learn.go`)
- Maintains 30-day rolling daily occurrence counters (`daily: {"YYYY-MM-DD": count}`).
- When `compare_baseline=true` is requested in `analyze_logs`:
  - Patterns exceeding $2\times$ historical daily average are flagged with `spike: true`.
  - Patterns dropping below $0.5\times$ historical daily average are flagged with `drop: true`.

---

## Guardrail Security Layer ("Tidak Ada Yang Keluar")

GoAgent v0.2 enforces a multi-layered guardrail boundary (`internal/guardrail/`):

```
Tool Execution  ──> [Secret Redaction] ──> [Truncation (8KB)] ──> LLM Context
                                                                        │
Telegram User   <── [Cap (3800)] <── [Leak Filter] <── [Markup Strip] <──┘
```

1. **Secret Redaction Pipeline (`secretredact.go`)**:
   - Redacts sensitive credentials in **all tool outputs** before entering LLM context and in **all replies** before transmission.
   - Detects API keys: OpenAI (`sk-`), GitHub (`ghp_`, `gho_`, `github_pat_`), AWS (`AKIA`), Slack (`xoxb-`, `xoxp-`), Google (`AIza`).
   - Detects Bearer tokens and JSON Web Tokens (JWT).
   - Redacts passwords in database connection strings (`postgres://`, `mysql://`, `mongodb://`, `redis://`, `amqp://`).
   - Replaces entire private key blocks (`-----BEGIN ... PRIVATE KEY-----`).
   - Redacts `password=`, `passwd=`, `secret=`, `token=`, `api_key=` assignments.
   - Replaces email addresses with `[REDACTED:email]`.
   - Validates credit card numbers with the Luhn checksum algorithm before redacting (`[REDACTED:card]`).
   - Preserves IPv4 addresses for log analysis by default, with optional masking (`x.x.x.<last-octet>`) via `GOAGENT_MASK_IP=true`.

2. **No-Leak Reply Filter (`replyfilter.go`)**:
   - Strips internal reasoning markup, HTML comments (`<!-- ... -->`), `<s>` thought tags, raw `system:` lines, and tool-call code blocks.
   - Replaces accidental system prompt disclosures ("You are GoAgent", "ATURAN", etc.) with safe placeholders.
   - Enforces a 3800-character cap per reply with a `_(truncated — type "more")_` footer.

3. **Command Guard (`commandguard.go`)**:
   - Blocklist permanently rejected commands even when `GOAGENT_ALLOW_SHELL=true`:
     `rm -rf`, `mkfs`, `dd if=.*of=/dev/`, `sudo`, `curl|sh`, `wget|sh`, `> /dev/sd*`, `chmod 777 /`, `shutdown`, `reboot`, `:(){ :|:& };:`.
   - Default safe mode restricts execution to read-only diagnostics (`ls`, `cat`, `grep`, `tail`, `head`, `ps`, `df`, `du`, `stat`, etc.) and forbids file redirection (`>`).

4. **Path Guard (`pathguard.go`)**:
   - Validates that all file operations resolve within authorized root directories (`GOAGENT_ALLOWED_ROOTS`).
   - Evaluates symlinks; symlinks escaping authorized roots are rejected.
   - Forbids reading `.env*` files by default unless `GOAGENT_ALLOW_ENV=true`.

---

## Agent Loop (Hermes-Grade)

- **Parallel Tool Execution**: When the LLM outputs multiple tool calls in a single step, GoAgent executes them concurrently using a goroutine worker pool (max 4 concurrent) while preserving the original deterministic slice order.
- **Tool Result Truncation**: Tool outputs are capped at 8,000 bytes with a truncation footer to avoid context blowing.
- **Context Budget Pruning**: If conversation history exceeds 60,000 characters, GoAgent automatically replaces the oldest tool outputs with `[tool result omitted]`, preserving user instructions and assistant conclusions.

---

## Configuration

Settings are loaded from `goagent.env` in the current working directory and can be overridden by system environment variables:

| Variable | Default | Description |
| :--- | :--- | :--- |
| `TELEGRAM_TOKEN` | *Required* | Telegram Bot API token obtained from [@BotFather](https://t.me/BotFather). |
| `LLM_BASE_URL` | `https://api.openai.com/v1` | OpenAI-compatible endpoint (OpenAI, Ollama, Groq, OpenRouter, vLLM). |
| `LLM_API_KEY` | *(empty)* | API key for the LLM provider. |
| `LLM_MODEL` | `gpt-4o-mini` | Model name for completions and function calling. |
| `GOAGENT_ADMIN_CHAT_ID` | *(empty)* | Optional Telegram chat ID. When set, only messages from this ID are answered. |
| `GOAGENT_ALLOW_SHELL` | `false` | When `true`, enables arbitrary shell execution (destructive blocklist still enforced). |
| `GOAGENT_MAX_ITER` | `8` | Maximum tool execution loop iterations per user prompt. |
| `GOAGENT_MASK_IP` | `false` | When `true`, masks IPv4 addresses to `x.x.x.<last-octet>`. |
| `GOAGENT_ALLOWED_ROOTS` | `cwd:/var/log:/tmp:~/Library/Logs:~/.goagent` | Colon-separated allowed filesystem roots for file and log tools. |
| `GOAGENT_ALLOW_ENV` | `false` | When `true`, permits reading `.env*` files. |

---

## Service Management (`deploy.sh`)

The `./deploy.sh` script automates cross-platform service management (macOS launchd and Linux systemd):

```bash
./deploy.sh build     # Compile binary to bin/goagent
./deploy.sh install   # Interactive setup and system service installation
./deploy.sh start     # Start background daemon
./deploy.sh stop      # Stop background daemon
./deploy.sh restart   # Restart background daemon
./deploy.sh status    # Check service status
./deploy.sh logs      # Stream live logs (tail -f /tmp/goagent.log or journalctl)
```

---

## License

MIT License. See [LICENSE](LICENSE) for details.
