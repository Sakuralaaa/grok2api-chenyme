# grok2api 账号注册机 sidecar
#
# 为什么单独一个镜像：
#   注册机要过 Cloudflare Turnstile，必须真浏览器（Camoufox/Firefox）有头运行，
#   体积约 1.2~1.5 GB。放进主镜像会拖垮 grok2api 的部署体积，所以拆成 sidecar。
#
# 注册机源码已内置于镜像 /opt/registrar-app（不存任何密钥，密钥全部走环境变量），
# 因此在 Zeabur 这类没有宿主机挂载的平台上也能直接跑。
# 本机 docker compose 仍可用 -v 覆盖该目录。
FROM python:3.12-slim-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PYTHONIOENCODING=utf-8 \
    TZ=Asia/Shanghai \
    REGISTRAR_APP_DIR=/opt/registrar-app \
    REGISTRAR_LISTEN_HOST=0.0.0.0 \
    REGISTRAR_LISTEN_PORT=8099 \
    REGISTRAR_OUT_DIR=/out

# Firefox + Xvfb 运行库。多架构（amd64/arm64）都用官方 python 基础镜像，
# 由 GitHub Actions 分别在原生 runner 上构建，不走 QEMU。
RUN apt-get update && apt-get install -y --no-install-recommends \
      xvfb xauth ca-certificates curl tini \
      libasound2 libatk1.0-0 libatk-bridge2.0-0 libcairo2 libcups2 \
      libdbus-1-3 libdbus-glib-1-2 libdrm2 libexpat1 libfontconfig1 \
      libfreetype6 libgbm1 libglib2.0-0 libgtk-3-0 libnspr4 libnss3 \
      libpango-1.0-0 libpangocairo-1.0-0 libx11-6 libx11-xcb1 libxcb1 \
      libxcb-shm0 libxcomposite1 libxdamage1 libxext6 libxfixes3 \
      libxrandr2 libxrender1 libxtst6 libxt6 libxss1 libxi6 libsqlite3-0 \
      libvulkan1 fonts-noto-cjk fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

ARG CAMOUFOX_VERSION=""
RUN if [ -n "$CAMOUFOX_VERSION" ]; then \
      pip install --no-cache-dir "camoufox[geoip]==${CAMOUFOX_VERSION}" curl_cffi; \
    else \
      pip install --no-cache-dir "camoufox[geoip]" curl_cffi; \
    fi \
    && python -m camoufox fetch \
    && python -c "import camoufox, curl_cffi; print('camoufox', camoufox.__version__, 'curl_cffi', curl_cffi.__version__)"

# 注册机驱动 + 本地 socks5 转发
COPY tools/account-registrar/relay.py /opt/account-registrar/relay.py
COPY tools/account-registrar/registrar_agent.py /opt/account-registrar/registrar_agent.py
COPY tools/account-registrar/entrypoint.sh /usr/local/bin/account-registrar-entrypoint
RUN chmod 0755 /usr/local/bin/account-registrar-entrypoint

# 注册机本体（不含密钥的源码 + config.example.json 模板）
COPY tools/account-registrar/app/ /opt/registrar-app/
RUN mkdir -p /out && chmod 0700 /out

# 内部 HTTP 接口就绪即视为健康；--once 模式下不监听，关掉健康检查由运行方式决定。
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${REGISTRAR_LISTEN_PORT}/healthz" >/dev/null || exit 1

# 需要写 /opt/registrar-app/config.json 并拉起浏览器，默认保持 root；
# 如需降权，请自行调整目录属主后改 USER。
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/account-registrar-entrypoint"]
