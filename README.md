# GoAgent v0.1

GoAgent is an open-source, general-purpose Telegram assistant built in pure Go that specializes in high-speed log analysis, self-learning pattern clustering, and system diagnostics. It connects OpenAI-compatible LLMs to your local environment using chunk-parallel log engines and safe diagnostic tools with zero external dependencies.

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
| `analyze_logs` | `path`, `top?`, `filter_regex?`, `min_count?` | High-speed chunk-parallel log analysis. Clusters logs into normalized templates, calculates severity distributions, flags anomalies, and updates self-learning pattern memory. |
| `read_log` | `path`, `tail?`, `filter_regex?` | Reads the most recent lines of a log file with optional regex filtering. |
| `learned_patterns` | `file?` | Retrieves historical pattern memory and frequency statistics across past analyses. |
| `read_file` | `path`, `offset?`, `limit?` | Reads lines from a file with pagination support. |
| `write_file` | `path`, `content` | Writes text content to a file, automatically creating parent directories. |
| `list_dir` | `path?` | Lists directory contents with file size, permissions, and timestamps. |
| `run_command` | `cmd`, `timeout_sec?` | Executes diagnostic shell commands. Guarded by a safe allowlist by default. |

---

## How Log Analysis Works

The log analysis engine (`internal/logskill/engine.go`) is engineered for speed on multi-core systems:

```
+---------------------------------------------------------------+
|                       Target Log File                         |
|   [Chunk 0: 0-8MB]   [Chunk 1: 8-16MB]   ...  [Chunk K: ... ] |
+---------------------------------------------------------------+
           |                    |                        |
           v                    v                        v
     +------------+       +------------+           +------------+
     | Goroutine0 |       | Goroutine1 |    ...    | GoroutineK |
     +------------+       +------------+           +------------+
           |                    |                        |
           +--------------------+------------------------+
                                |
                                v
               +----------------------------------+
               |  Template Normalization Pipeline |
               |   1. Timestamps -> {DATE}        |
               |   2. Time       -> {TIME}        |
               |   3. IPv4       -> {IP}          |
               |   4. UUID/Hex   -> {UUID}/{HEX}  |
               |   5. Numbers    -> {N}           |
               |   6. Quoted Str -> {STR}         |
               |   7. File Paths -> {PATH}        |
               +----------------------------------+
                                |
                                v
               +----------------------------------+
               |      Merge & Pattern Store       |
               |   - Total line & level counters  |
               |   - Top N frequent templates     |
               |   - Anomaly detection & learning |
               +----------------------------------+
```

1. **Chunk Splitting**: Files are split into $K$ byte-range chunks ($K = \min(8, \text{size}/8\text{MB})$). Each worker seeks to its range and advances to the next newline boundary to guarantee that every line is read exactly once without locks.
2. **Template Normalization**: Dynamic variables (timestamps, IP addresses, UUIDs, hex tokens, numbers, quoted strings, file paths) are replaced with unified tokens.
3. **Aggregation & Anomaly Detection**: Chunks are merged into top frequency statistics. Patterns with frequency $\le \text{min\_count}$ or unseen in past analyses are flagged as anomalies.

---

## Self-Learning Patterns

GoAgent remembers recurring log patterns across sessions:

- **Storage**: Pattern knowledge is saved under `~/.goagent/patterns/<sanitized-filename>.json`.
- **Automatic Learning**: Every execution of `analyze_logs` loads the existing store, increments historical occurrence counters, updates `last_seen`, and marks previously unseen templates with `new: true`.
- **Atomic Writes**: Pattern stores are written to a temporary file and committed using `os.Rename` to guarantee data integrity.
- **Manual Reset**: To reset pattern memory for a log file, simply delete its corresponding JSON file in `~/.goagent/patterns/`.

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
| `GOAGENT_ALLOW_SHELL` | `false` | When `true`, enables arbitrary shell execution. When `false`, enforces safe allowlist. |
| `GOAGENT_MAX_ITER` | `8` | Maximum tool execution loop iterations per user prompt. |

---

## Security Notes

1. **Safe Shell Allowlist**: By default (`GOAGENT_ALLOW_SHELL=false`), `run_command` only permits read-only diagnostic utilities:
   `ls`, `cat`, `tail`, `head`, `grep`, `wc`, `du`, `df`, `ps`, `find`, `stat`, `file`, `date`, `uname`.
   Any attempt to run unapproved binaries or use output redirection (`>`) is rejected.
2. **Access Control**: Set `GOAGENT_ADMIN_CHAT_ID` in `goagent.env` to restrict the bot to your personal Telegram account so unauthorized users cannot invoke tools.

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
