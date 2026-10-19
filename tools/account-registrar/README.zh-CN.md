# 账号注册机 sidecar

把「注册机」以独立容器的方式接进 grok2api：注册机负责跑浏览器拿 SSO，
grok2api 负责入库、同步和质检。

## 为什么是 sidecar，而不是塞进主镜像

注册机要过 Cloudflare Turnstile，必须真浏览器（Camoufox / Firefox）**有头**运行，
还要 Xvfb 提供虚拟显示，镜像 1.2~1.5 GB。塞进主镜像会让 grok2api 的部署体积翻十几倍，
所以拆成独立镜像，由 compose 的 `registrar` profile 按需启动，默认不跑。

## 镜像怎么来

由 `.github/workflows/account-registrar-image.yml` 在 GitHub Actions 上构建：

- `linux/amd64` 跑在 `ubuntu-24.04`
- `linux/arm64` 跑在 `ubuntu-24.04-arm`（原生 ARM，不走 QEMU）
- 两个架构各自推 `<tag>-amd64` / `<tag>-arm64`，最后合并成 manifest list

发布地址：`ghcr.io/<owner>/<repo>/account-registrar:<tag>`
挂在主仓库的 namespace 下，和主镜像同一个包来源，拉取时不用换仓库。
`latest` 只在 `main` 分支推送时生成；其它分支用分支名或 `sha-<短哈希>`。

拉取前先登录（包默认私有）：

```bash
echo "$GHCR_PAT" | docker login ghcr.io -u <你的用户名> --password-stdin
docker pull ghcr.io/<owner>/<repo>/account-registrar:latest
```

## 镜像里有什么 / 没有什么

有：Python 3.12、Camoufox 运行时 + 已下载的 Firefox、Xvfb、CJK 字体、中继脚本。

**没有注册机源码**。注册机本体在运行时挂载进来：

```bash
-v /path/to/grokRegister-cpa:/opt/registrar-app
```

镜像只做三件事：驱动 `/opt/registrar-app` 里的 `grok_register_ttk.py cli`、
把产出的 `accounts/*.txt`（`邮箱----密码----SSO`）读出来、交给后端或写到 `/out`。

## 先验证「容器里能不能注册成功」

不带任何后端，跑一次性模式：

```bash
docker run --rm -it \
  -v /path/to/grokRegister-cpa:/opt/registrar-app \
  -v "$PWD/out:/out" \
  -e REGISTRAR_COUNT=1 \
  -e REGISTRAR_DOMAINS=edu.sakural.app \
  ghcr.io/<owner>/<repo>/account-registrar:latest --once
```

成功时 stdout 是任务 JSON（`"status": "done"`、`"imported": 1`），
凭据写进 `./out/registered-accounts.txt`（权限 600）。

失败时 `lastError` 会带注册机输出的尾部，重点看是不是：

| 现象 | 原因 |
|---|---|
| `appDir` 不存在 | 挂载路径写错，或 `REGISTRAR_APP_DIR` 没指到 |
| `Cloudflare 拦截` / `Turnstile` 相关 | 出口 IP 脏，换节点重试 |
| `注册机未产出新账号 (exit=...)` | 注册机自身报错，看 lastError 里的尾部日志 |

## 用代理的情况

带用户名密码的 socks5 **不能**直接给 Playwright：它会抛
`Browser type launch: Browser does not support socks5 proxy authentication`。

所以镜像内置了本地免认证转发：填上游 URL 就行，入口脚本会自动起转发并把
`REGISTRAR_PROXY` 指过去。

```bash
-e RELAY_UPSTREAM='socks5://user:pass@host:port'
```

注意两点：

1. 上游若是 WARP 之类的落地，往往只接受「域名型」SOCKS5 请求，
   用 `socks5://`（本地解析成 IPv4）会被直接断连——转发脚本统一按域名形式下发。
2. 需要地理位置/时区匹配时额外给 `-e CAMOUFOX_GEOIP_IP=<上游出口 IPv4>`，
   否则 Camoufox 的 `geoip=True` 会经代理去查公网 IP，撞上上面两个问题就报
   `InvalidIP: Failed to get IP address`。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `REGISTRAR_APP_DIR` | `/opt/registrar-app` | 注册机目录（挂载点） |
| `REGISTRAR_COUNT` | `1` | 本次注册数量 |
| `REGISTRAR_DOMAINS` | 空 | 收信域名，逗号分隔 |
| `REGISTRAR_OUT_DIR` | `/out` | 一次性模式的凭据输出目录 |
| `REGISTRAR_PROXY` | 空 | 给注册机用的代理；留空则直连 |
| `RELAY_UPSTREAM` | 空 | 带认证 socks5 上游，填了就自动起转发 |
| `CAMOUFOX_GEOIP_IP` | 空 | 指定的出口 IP，用于时区/语言匹配 |
| `REGISTRAR_JOB_TIMEOUT` | `900` | 单次注册超时（秒） |
| `GROK2API_BASE_URL` | 空 | 填了就走「注册完推给 grok2api」，不落盘 |

## 两种运行模式

**一次性（`--once`）**：注册完退出，适合验证链路是否通。

**常驻服务**：不传 `--once` 就起 HTTP 服务，由 grok2api 管理端下发任务。
需要 `bootstrap.json` 提供内部令牌，接口：

```
GET  /healthz
POST /jobs                {"count":1,"domains":["..."]}
GET  /jobs/{id}
POST /jobs/{id}/cancel
```

## 安全提醒

- 容器里会写出明文 SSO（`/out/registered-accounts.txt`，权限 600）。用完记得清。
- 日志里不会打印密码或 SSO；但注册机自身的输出会落到 `lastError`，注意别外传。
- 这个镜像默认以 root 运行（要写挂载目录的 `config.json` 并拉起浏览器）。
  要降权的话，先确保挂载目录属主正确，再自行加 `USER`。
- 邮箱服务的 admin key 会进注册机的 `config.json`，属于敏感信息，别提交进仓库。
