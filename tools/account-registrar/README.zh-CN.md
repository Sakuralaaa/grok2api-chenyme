# 账号注册机 sidecar

把「注册机」以独立容器的方式接进 grok2api：注册机负责跑浏览器拿 SSO，
grok2api 负责入库、同步和质检。管理端账号页会出现一个「自动补号」按钮。

## 为什么是独立镜像

注册机要过 Cloudflare Turnstile，必须真浏览器（Camoufox / Firefox）**有头**运行，
还要 Xvfb 提供虚拟显示，镜像 1.2~1.5 GB。塞进主镜像会让 grok2api 的部署体积翻十几倍，
所以拆成独立镜像：`ghcr.io/<owner>/<repo>/account-registrar:<tag>`。

## 镜像里有什么

- Python 3.12 + Camoufox（含已下载的 Firefox）+ Xvfb + CJK 字体
- 注册机源码（`/opt/registrar-app`，只含 `config.example.json` 模板，**不含任何密钥**）
- 本地 socks5 转发脚本
- 内部 HTTP 接口（默认 `:8099`）

密钥一律通过环境变量注入，运行时会临时生成 `config.json`，任务结束自动清理。

## 内部接口

```
GET  /healthz                      健康检查（不需要令牌）
POST /jobs                         {"count":1,"domains":["example.com"]}
GET  /jobs/{id}                    任务快照
POST /jobs/{id}/cancel             取消
```

主程序通过 `Authorization: Bearer <内部令牌>` 调用；sidecar 回传凭据走主程序的
`POST /api/internal/v1/account-registrar/accounts`（同一令牌）。
**所有响应都不包含密码或 SSO。**

## 令牌怎么来（两种模式）

| 模式 | 适用 | 做法 |
|---|---|---|
| 共享卷 | 同一台机器的 docker compose | 主程序把令牌写进 `registrar_state` 卷的 `bootstrap.json`，sidecar 读取。零配置 |
| 环境变量 | Zeabur / K8s 等多容器平台 | 两侧注入同一个 `REGISTRAR_TOKEN`；主程序侧变量名是 `GROK2API_ACCOUNT_REGISTRAR_TOKEN` |

环境变量模式下主程序**不会**写 `bootstrap.json`，因此不依赖任何共享卷。

生成一个令牌：

```bash
openssl rand -hex 32
```

## 部署方式一：本机 docker compose

```bash
docker login ghcr.io -u <你的用户名>     # PAT 需要 read:packages；或把包设为 public

cat > .env <<'EOF'
REGISTRAR_EMAIL_PROVIDER=cloudflare
REGISTRAR_CLOUDFLARE_API_BASE=https://你的邮箱API
REGISTRAR_CLOUDFLARE_API_KEY=你的管理密钥
REGISTRAR_DOMAINS=edu.example.com
EOF

docker compose --profile registrar up -d
```

`config.yaml` 里打开开关：

```yaml
accountRegistrar:
  enabled: true
  baseURL: "http://account-registrar:8099"
  callbackURL: "http://grok2api:8000"
  domains: ["edu.example.com"]
```

## 部署方式二：Zeabur

Zeabur 每个服务是独立容器，卷不跨服务共享，所以走**环境变量模式**。

### 1. 让 Zeabur 能拉到镜像

GHCR 包默认私有。二选一：

- 把包设为 public：GitHub → 你的 Packages → `account-registrar` → Package settings → Change visibility
- 或在 Zeabur 添加镜像服务时填入 GHCR 用户名 + PAT

### 2. 在同一个项目里新增服务

Zeabur 控制台 → 项目 `grok2api` → Add Service → **Docker Image** →
填 `ghcr.io/<owner>/<repo>/account-registrar:latest`（建议用 `sha-xxxx` 锁定版本）。

端口填 **8099**，类型 HTTP。**不需要绑定公网域名**（仅服务间调用）。

资源建议 ≥ 2 GB 内存（要跑 Firefox）。

### 3. 注册机服务的环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `REGISTRAR_INTERNAL_TOKEN` | ✅ | 与主程序同一个令牌 |
| `GROK2API_BASE_URL` | ✅ | 主程序内网地址，例 `http://grok2api:8000` |
| `REGISTRAR_EMAIL_PROVIDER` | ✅ | 目前用 `cloudflare` |
| `REGISTRAR_CLOUDFLARE_API_BASE` | ✅ | 自建邮箱 API 根地址 |
| `REGISTRAR_CLOUDFLARE_API_KEY` | ✅ | 邮箱 API 管理密钥 |
| `REGISTRAR_DOMAINS` | ✅ | 收信域名，逗号分隔 |
| `RELAY_UPSTREAM` | 可选 | 带认证 socks5 上游；填了会自动起本地转发并让浏览器走它 |
| `CAMOUFOX_GEOIP_IP` | 可选 | 出口 IPv4，用于时区/语言匹配（走代理时建议填） |
| `REGISTRAR_MAX_COUNT` | 可选 | 单次补号上限，默认 5 |

### 4. 主程序服务的环境变量

| 变量 | 说明 |
|---|---|
| `GROK2API_ACCOUNT_REGISTRAR_URL` | 注册机内网地址，例 `http://account-registrar:8099`。设置后自动视为启用 |
| `GROK2API_ACCOUNT_REGISTRAR_TOKEN` | 与注册机同一个令牌 |
| `GROK2API_ACCOUNT_REGISTRAR_DOMAINS` | 可选，面板下拉框的默认域名 |

### 5. 验证

主程序容器里：

```bash
npx zeabur@latest service exec --id <grok2api服务ID> -- \
  wget -qO- --header="Authorization: Bearer $TOKEN" http://account-registrar:8099/healthz
```

返回 `{"data":{"ok":true,...}}` 即通。然后管理端账号页的「自动补号」按钮应当可用。

## 内网地址怎么填

同一项目内的服务可以直接用**服务名**当主机名（`http://account-registrar:8099`）。
如果解析不到，在控制台该服务的网络页找到内部域名替换；最后的兜底是给它绑一个域名，
把 `GROK2API_ACCOUNT_REGISTRAR_URL` 填成 `https://<域名>`（走公网 ingress 也能用）。

## 常见问题

| 现象 | 原因 |
|---|---|
| 面板按钮灰着，提示 sidecar 不可用 | 主程序没配 `GROK2API_ACCOUNT_REGISTRAR_URL`，或网络不通 |
| `registrarUnauthorized` | 两侧令牌不一致 |
| `缺少邮件服务环境变量: ...` | 邮箱 API 凭据没配 |
| 注册机日志出现 `InvalidIP: Failed to get IP address` | 用了代理但没给 `CAMOUFOX_GEOIP_IP`，Camoufox 经代理查公网 IP 失败 |
| `Browser does not support socks5 proxy authentication` | 把带账号密码的 socks5 直接给了 `REGISTRAR_PROXY`；改用 `RELAY_UPSTREAM` |
| `Turnstile` / `Cloudflare 拦截` | 出口 IP 脏，换节点重试 |
| 容器 OOM | 内存给太少，Firefox 起不来 |

## 安全提醒

- 镜像内不含任何密钥；密钥只存在于环境变量与运行期临时文件（`config.json` 权限 0600）。
- `--once` 模式会把凭据写到 `/out/registered-accounts.txt`（0600），用完请清理。
- 日志不会打印密码或 SSO，但注册机的输出尾部会进入任务的 `lastError`。
- 容器默认以 root 运行（要写 `/opt/registrar-app/config.json` 并拉起浏览器）。

## 注册机源码来源

`app/` 目录来自你自己的注册机项目，已剔除账号文件、日志、缓存与任何密钥，
只保留源码、`LICENSE`、`README.md` 和一份脱敏的 `config.example.json`。
