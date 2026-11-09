#!/usr/bin/env bash
set -euo pipefail

# Determine script and project paths
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$DIR/bin/goagent"
ENV_FILE="$DIR/goagent.env"
OS="$(uname -s)"

echo_info() {
    echo -e "\033[1;34m==>\033[0m \033[1m$1\033[0m"
}

echo_success() {
    echo -e "\033[1;32m==>\033[0m \033[1m$1\033[0m"
}

echo_error() {
    echo -e "\033[1;31mError:\033[0m $1" >&2
}

cmd_build() {
    echo_info "Building GoAgent binary..."
    mkdir -p "$DIR/bin"
    (cd "$DIR" && go build -o "$BIN" ./cmd/goagent)
    echo_success "Build successful: $BIN"
}

cmd_install_service_mac() {
    local plist_dir="$HOME/Library/LaunchAgents"
    local plist_file="$plist_dir/com.goagent.plist"
    local log_file="/tmp/goagent.log"

    echo_info "Installing macOS launchd agent..."
    mkdir -p "$plist_dir"

    cat <<EOF > "$plist_file"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.goagent</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$DIR</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$log_file</string>
    <key>StandardErrorPath</key>
    <string>$log_file</string>
</dict>
</plist>
EOF

    echo_info "Registering service with launchctl..."
    launchctl unload "$plist_file" 2>/dev/null || true
    launchctl load -w "$plist_file"

    echo_success "GoAgent macOS service installed and loaded ($plist_file)."
    echo_info "Service log file: $log_file"
}

cmd_install_service_linux() {
    local systemd_dir="$HOME/.config/systemd/user"
    local service_file="$systemd_dir/goagent.service"

    echo_info "Installing Linux systemd user service..."
    mkdir -p "$systemd_dir"

    cat <<EOF > "$service_file"
[Unit]
Description=GoAgent Log Analysis Agent
After=network.target

[Service]
Type=simple
WorkingDirectory=$DIR
ExecStart=$BIN
Restart=always
RestartSec=5
EnvironmentFile=-$ENV_FILE

[Install]
WantedBy=default.target
EOF

    echo_info "Reloading systemd user daemon..."
    systemctl --user daemon-reload
    systemctl --user enable --now goagent.service

    echo_success "GoAgent systemd user service installed and started ($service_file)."
    echo_info "View logs with: journalctl --user -u goagent -f"
}

cmd_install() {
    echo_info "Starting GoAgent installation..."

    local configure_env=true
    if [ -f "$ENV_FILE" ]; then
        if [ -t 0 ]; then
            read -r -p "Existing goagent.env found. Use existing configuration? [Y/n]: " use_existing
            case "$use_existing" in
                [nN][oO]|[nN]) configure_env=true ;;
                *) configure_env=false ;;
            esac
        else
            configure_env=false
        fi
    fi

    if [ "$configure_env" = true ]; then
        echo_info "Configuring environment..."
        local tg_token=""
        local llm_base="https://api.openai.com/v1"
        local llm_key=""
        local llm_model="gpt-4o-mini"
        local admin_id=""

        if [ -t 0 ]; then
            while [ -z "$tg_token" ]; do
                read -r -p "Enter TELEGRAM_TOKEN (from @BotFather): " tg_token
                if [ -z "$tg_token" ]; then
                    echo_error "TELEGRAM_TOKEN is required!"
                fi
            done

            read -r -p "Enter LLM_BASE_URL [https://api.openai.com/v1]: " user_llm_base
            if [ -n "$user_llm_base" ]; then llm_base="$user_llm_base"; fi

            read -r -p "Enter LLM_API_KEY: " llm_key

            read -r -p "Enter LLM_MODEL [gpt-4o-mini]: " user_llm_model
            if [ -n "$user_llm_model" ]; then llm_model="$user_llm_model"; fi

            read -r -p "Enter GOAGENT_ADMIN_CHAT_ID (optional, leave empty for any chat): " admin_id
        else
            echo_error "Non-interactive environment and no goagent.env found. Please copy goagent.env.example to goagent.env first."
            exit 1
        fi

        cat <<EOF > "$ENV_FILE"
# GoAgent Configuration
TELEGRAM_TOKEN=$tg_token
LLM_BASE_URL=$llm_base
LLM_API_KEY=$llm_key
LLM_MODEL=$llm_model
GOAGENT_ADMIN_CHAT_ID=$admin_id
GOAGENT_ALLOW_SHELL=false
GOAGENT_MAX_ITER=8
EOF
        echo_success "Wrote configuration to $ENV_FILE"
    else
        echo_info "Using existing $ENV_FILE"
    fi

    cmd_build

    if [ "$OS" = "Darwin" ]; then
        cmd_install_service_mac
    elif [ "$OS" = "Linux" ]; then
        cmd_install_service_linux
    else
        echo_error "Unsupported operating system: $OS. Built binary only."
        exit 1
    fi

    echo_success "Installation complete! GoAgent is running."
}

cmd_start() {
    echo_info "Starting GoAgent service..."
    if [ "$OS" = "Darwin" ]; then
        local plist_file="$HOME/Library/LaunchAgents/com.goagent.plist"
        if [ ! -f "$plist_file" ]; then
            echo_error "Service not installed. Run './deploy.sh install' first."
            exit 1
        fi
        launchctl load -w "$plist_file" 2>/dev/null || launchctl start com.goagent
        echo_success "Service started."
    elif [ "$OS" = "Linux" ]; then
        systemctl --user start goagent
        echo_success "Service started."
    fi
}

cmd_stop() {
    echo_info "Stopping GoAgent service..."
    if [ "$OS" = "Darwin" ]; then
        local plist_file="$HOME/Library/LaunchAgents/com.goagent.plist"
        launchctl unload "$plist_file" 2>/dev/null || true
        echo_success "Service stopped."
    elif [ "$OS" = "Linux" ]; then
        systemctl --user stop goagent
        echo_success "Service stopped."
    fi
}

cmd_restart() {
    cmd_stop
    sleep 1
    cmd_start
}

cmd_status() {
    echo_info "Checking GoAgent service status..."
    if [ "$OS" = "Darwin" ]; then
        if launchctl list | grep -q "com.goagent"; then
            echo_success "GoAgent is running (com.goagent registered in launchd)."
            launchctl list | grep "com.goagent"
        else
            echo_info "GoAgent is not running in launchd."
        fi
    elif [ "$OS" = "Linux" ]; then
        systemctl --user status goagent
    fi
}

cmd_logs() {
    echo_info "Streaming GoAgent logs..."
    if [ "$OS" = "Darwin" ]; then
        local log_file="/tmp/goagent.log"
        if [ ! -f "$log_file" ]; then
            touch "$log_file"
        fi
        tail -n 100 -f "$log_file"
    elif [ "$OS" = "Linux" ]; then
        journalctl --user -u goagent -n 100 -f
    fi
}

usage() {
    echo "GoAgent Management Script"
    echo ""
    echo "Usage: $0 {build|install|start|stop|restart|status|logs}"
    echo ""
    echo "Commands:"
    echo "  build    - Compile bin/goagent"
    echo "  install  - Interactively configure env, compile binary, and register OS service"
    echo "  start    - Start background service (launchd on macOS, systemd on Linux)"
    echo "  stop     - Stop background service"
    echo "  restart  - Restart background service"
    echo "  status   - View service status"
    echo "  logs     - Stream service logs"
    exit 1
}

case "${1:-}" in
    build)
        cmd_build
        ;;
    install)
        cmd_install
        ;;
    start)
        cmd_start
        ;;
    stop)
        cmd_stop
        ;;
    restart)
        cmd_restart
        ;;
    status)
        cmd_status
        ;;
    logs)
        cmd_logs
        ;;
    *)
        usage
        ;;
esac
