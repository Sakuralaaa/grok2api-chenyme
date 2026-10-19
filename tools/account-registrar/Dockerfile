# grok2api 账号注册机 sidecar
#
# 为什么单独一个镜像：
#   注册机要过 Cloudflare Turnstile，必须真浏览器（Camoufox/Firefox）有头运行，
#   体积约 1.2~1.5 GB。放进主镜像会拖垮 grok2api 的部署体积，所以拆成 sidecar，
#   由 docker compose 的 registrar profile 按需启动。
#
# 注册机本体不进镜像：运行时挂载到 /opt/registrar-app
#   -v /path/to/grokRegister-cpa:/opt/registrar-app
FROM python:3.12-slim-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PYTHONIOENCODING=utf-8 \
    TZ=Asia/Shanghai

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
      pip install --no-cache-dir "camoufox[geoip]==${CAMOUFOX_VERSION}"; \
    else \
      pip install --no-cache-dir "camoufox[geoip]"; \
    fi \
    && python -m camoufox fetch \
    && python -c "import camoufox, pathlib; print('camoufox', camoufox.__version__)"

COPY tools/account-registrar/relay.py /opt/account-registrar/relay.py
COPY tools/account-registrar/registrar_agent.py /opt/account-registrar/registrar_agent.py
COPY tools/account-registrar/entrypoint.sh /usr/local/bin/account-registrar-entrypoint
RUN chmod 0755 /usr/local/bin/account-registrar-entrypoint

# 注册机挂载点 + 一次性运行的产物目录
RUN mkdir -p /opt/registrar-app /out
VOLUME ["/opt/registrar-app", "/out"]

# 需要写注册机目录下的 config.json 并启动浏览器，这里保持 root；
# 如需降权，请自行调整挂载目录属主后改 USER。
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/account-registrar-entrypoint"]
