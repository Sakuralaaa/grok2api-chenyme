#!/bin/sh
# grok2api 账号注册机 sidecar 入口
# 负责：可选启动本地 socks5 转发 -> 在 Xvfb 虚拟显示下运行注册机
set -e

log() { echo "[entrypoint] $*" >&2; }

# 上游带认证 socks5 无法被 Playwright 直接使用，先在本机开免认证入口再指过去
if [ -n "${RELAY_UPSTREAM:-}" ] && [ -z "${REGISTRAR_PROXY:-}" ]; then
  log "启动本地 socks5 转发 ${RELAY_LISTEN_HOST:-127.0.0.1}:${RELAY_LISTEN_PORT:-10800}"
  python3 /opt/account-registrar/relay.py &
  relay_pid=$!
  # 等转发口就绪，最多 10 秒
  i=0
  while [ "$i" -lt 20 ]; do
    if python3 - "$RELAY_LISTEN_HOST" "$RELAY_LISTEN_PORT" <<'PY'
import socket, sys
host = sys.argv[1] or "127.0.0.1"
port = int(sys.argv[2] or 10800)
s = socket.socket()
s.settimeout(1)
try:
    s.connect((host, port))
except OSError:
    raise SystemExit(1)
finally:
    s.close()
PY
    then
      export REGISTRAR_PROXY="socks5://${RELAY_LISTEN_HOST:-127.0.0.1}:${RELAY_LISTEN_PORT:-10800}"
      log "转发就绪，REGISTRAR_PROXY=${REGISTRAR_PROXY}"
      break
    fi
    i=$((i + 1))
    sleep 0.5
  done
  if [ -z "${REGISTRAR_PROXY:-}" ]; then
    log "转发启动失败，退出"
    kill "$relay_pid" 2>/dev/null || true
    exit 1
  fi
fi

# 注册机必须有头运行（Turnstile 反检测），用 Xvfb 提供虚拟显示
exec xvfb-run -a -s "-screen 0 1920x1080x24" \
  python3 /opt/account-registrar/registrar_agent.py "$@"
