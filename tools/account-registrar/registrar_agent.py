#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""grok2api 账号注册机 sidecar。

职责边界（刻意做窄）：
  1. 接收 grok2api 下发的「补号」任务；
  2. 驱动外部注册机 CLI 完成注册，产出 邮箱----密码----SSO；
  3. 把凭据 POST 回 grok2api 的内部接口，由主程序负责入库/同步/质检；
  4. 对外只暴露任务状态，**永不回传密码或 SSO**。

只用标准库。注册机本体不在此仓库内：通过 bootstrap.json 的 app_dir 指向。
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import threading
import time
import traceback
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

DEFAULT_BOOTSTRAP = "/var/lib/grok2api-account-registrar/bootstrap.json"
MAX_JOB_ITEMS = 50
ACCOUNT_FILE_SUFFIX = ".txt"
REGISTRAR_ENTRY = "grok_register_ttk.py"
LOG_LIMIT = 4000


def log(message: str) -> None:
    sys.stderr.write("[registrar] %s\n" % message)
    sys.stderr.flush()


# --------------------------------------------------------------------------- 配置


class Config:
    def __init__(self, payload: dict) -> None:
        cfg = payload.get("config") or {}
        self.enabled = bool(payload.get("enabled"))
        self.internal_token = str(payload.get("internal_token") or "")
        self.base_url = str(cfg.get("base_url") or "").rstrip("/")
        self.app_dir = str(cfg.get("app_dir") or "")
        self.domains = [str(v).strip() for v in (cfg.get("domains") or []) if str(v).strip()]
        self.proxy = str(cfg.get("proxy") or "").strip()
        self.geoip_ip = str(cfg.get("geoip_ip") or "").strip()
        self.max_count = max(1, min(int(cfg.get("max_count") or 5), MAX_JOB_ITEMS))
        self.job_timeout_seconds = max(120, int(cfg.get("job_timeout_seconds") or 900))
        self.python = str(cfg.get("python") or sys.executable)
        self.email_provider_domains = str(cfg.get("email_provider_domains") or ",".join(self.domains))

    @property
    def import_url(self) -> str:
        return self.base_url + "/api/internal/v1/account-registrar/accounts"

    @classmethod
    def from_env(cls) -> "Config":
        """一次性运行模式：不依赖 bootstrap.json，全部从环境变量取。"""
        domains = [d.strip() for d in os.environ.get("REGISTRAR_DOMAINS", "").split(",") if d.strip()]
        return cls({
            "enabled": True,
            "internal_token": os.environ.get("REGISTRAR_INTERNAL_TOKEN", ""),
            "config": {
                "base_url": os.environ.get("GROK2API_BASE_URL", ""),
                "app_dir": os.environ.get("REGISTRAR_APP_DIR", "/opt/registrar-app"),
                "domains": domains,
                "proxy": os.environ.get("REGISTRAR_PROXY", ""),
                "geoip_ip": os.environ.get("CAMOUFOX_GEOIP_IP", ""),
                "max_count": os.environ.get("REGISTRAR_COUNT", "5"),
                "job_timeout_seconds": os.environ.get("REGISTRAR_JOB_TIMEOUT", "900"),
            },
        })


def load_config() -> Config:
    path = os.environ.get("REGISTRAR_BOOTSTRAP", DEFAULT_BOOTSTRAP)
    with open(path, encoding="utf-8") as handle:
        return Config(json.load(handle))


# --------------------------------------------------------------------------- 任务状态


class Job:
    def __init__(self, count: int, domains: list[str]) -> None:
        self.id = uuid.uuid4().hex
        self.count = count
        self.domains = domains
        self.status = "queued"
        self.created = 0
        self.imported = 0
        self.failed = 0
        self.started_at = 0.0
        self.finished_at = 0.0
        self.last_error = ""
        self.items: list[dict] = []
        self.canceled = False
        self.lock = threading.Lock()

    def snapshot(self) -> dict:
        with self.lock:
            return {
                "jobId": self.id,
                "status": self.status,
                "count": self.count,
                "created": self.created,
                "imported": self.imported,
                "failed": self.failed,
                "startedAt": int(self.started_at),
                "finishedAt": int(self.finished_at),
                "lastError": self.last_error,
                "items": [dict(item) for item in self.items],
            }

    def update(self, **kwargs) -> None:
        with self.lock:
            for key, value in kwargs.items():
                setattr(self, key, value)


class JobStore:
    def __init__(self) -> None:
        self._jobs: dict[str, Job] = {}
        self._lock = threading.Lock()

    def add(self, job: Job) -> None:
        with self._lock:
            self._jobs[job.id] = job
            # 只保留最近 32 个任务，避免长期运行内存堆积
            if len(self._jobs) > 32:
                oldest = sorted(self._jobs.values(), key=lambda j: j.started_at)[0]
                self._jobs.pop(oldest.id, None)

    def get(self, job_id: str) -> Job | None:
        with self._lock:
            return self._jobs.get(job_id)


# --------------------------------------------------------------------------- 注册机驱动


def snapshot_accounts(app_dir: str) -> dict[str, float]:
    root = os.path.join(app_dir, "accounts")
    result: dict[str, float] = {}
    if not os.path.isdir(root):
        return result
    for name in os.listdir(root):
        if not name.endswith(ACCOUNT_FILE_SUFFIX):
            continue
        try:
            result[name] = os.path.getmtime(os.path.join(root, name))
        except OSError:
            continue
    return result


def read_credentials(app_dir: str, name: str) -> tuple[str, str, str] | None:
    path = os.path.join(app_dir, "accounts", name)
    try:
        with open(path, encoding="utf-8") as handle:
            raw = handle.read().strip()
    except OSError:
        return None
    parts = [part.strip() for part in raw.split("----")]
    if len(parts) < 3:
        return None
    email, password, sso = parts[0], parts[1], parts[2]
    if not email or not sso:
        return None
    return email, password, sso


def write_registrar_config(app_dir: str, count: int, domains: list[str], proxy: str) -> str:
    """备份并改写注册机 config.json，返回备份路径。"""
    path = os.path.join(app_dir, "config.json")
    backup = path + ".registrar-sidecar.bak"
    if os.path.exists(path) and not os.path.exists(backup):
        shutil.copy2(path, backup)
    with open(path, encoding="utf-8") as handle:
        payload = json.load(handle)
    payload["register_count"] = max(1, count)
    payload["register_workers"] = 1
    payload["cpa_auto_add"] = False
    if domains:
        payload["defaultDomains"] = ",".join(domains)
    payload["proxy"] = proxy
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=4)
    return backup


def restore_registrar_config(app_dir: str, backup: str) -> None:
    path = os.path.join(app_dir, "config.json")
    try:
        if os.path.exists(backup):
            shutil.move(backup, path)
    except OSError as exc:
        log("恢复注册机配置失败: %r" % (exc,))


def run_registrar_cli(config: Config, env_extra: dict[str, str], timeout: int) -> tuple[int, str]:
    env = dict(os.environ)
    env.update(env_extra)
    env.setdefault("PYTHONUNBUFFERED", "1")
    env.setdefault("PYTHONIOENCODING", "utf-8")
    process = subprocess.run(  # noqa: S603 - 参数固定，无 shell
        [config.python, "-u", REGISTRAR_ENTRY, "cli"],
        cwd=config.app_dir,
        input="start\n",
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=timeout,
        env=env,
    )
    tail = (process.stdout or "")[-LOG_LIMIT:]
    return process.returncode, tail


def post_credentials(config: Config, email: str, password: str, sso: str) -> tuple[bool, str]:
    body = json.dumps({"email": email, "password": password, "sso": sso}).encode("utf-8")
    request = urllib.request.Request(config.import_url, data=body, method="POST")
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", "Bearer " + config.internal_token)
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            payload = json.loads(response.read().decode("utf-8", "replace") or "{}")
            data = payload.get("data") or {}
            if data.get("imported"):
                return True, ""
            return False, str(data.get("reason") or "not imported")
    except urllib.error.HTTPError as exc:
        detail = exc.read()[:200].decode("utf-8", "replace")
        return False, "HTTP %s %s" % (exc.code, detail)
    except Exception as exc:  # noqa: BLE001 - 上报而不是崩溃
        return False, repr(exc)


def save_local_copy(config: Config, email: str, password: str, sso: str) -> str:
    """一次性运行模式：把凭据写到挂载目录，供人工取用。"""
    out_dir = os.environ.get("REGISTRAR_OUT_DIR", "/out")
    try:
        os.makedirs(out_dir, exist_ok=True)
    except OSError as exc:
        log("创建输出目录失败: %r" % (exc,))
        return ""
    path = os.path.join(out_dir, "registered-accounts.txt")
    try:
        with open(path, "a", encoding="utf-8") as handle:
            handle.write("%s----%s----%s\n" % (email, password, sso))
        os.chmod(path, 0o600)
    except OSError as exc:
        log("写入输出文件失败: %r" % (exc,))
        return ""
    return path


def run_job(job: Job, config: Config) -> None:
    job.update(status="running", started_at=time.time())
    before = snapshot_accounts(config.app_dir)
    backup = ""
    try:
        if not os.path.isdir(config.app_dir):
            raise RuntimeError("注册机目录不存在: %s" % config.app_dir)
        backup = write_registrar_config(config.app_dir, job.count, job.domains, config.proxy)
        env_extra = {}
        if config.geoip_ip:
            env_extra["CAMOUFOX_GEOIP_IP"] = config.geoip_ip
        code, tail = run_registrar_cli(config, env_extra, config.job_timeout_seconds)
        after = snapshot_accounts(config.app_dir)
        fresh = [name for name, mtime in after.items() if before.get(name) != mtime]
        job.update(created=len(fresh))
        for name in fresh:
            if job.canceled:
                break
            credentials = read_credentials(config.app_dir, name)
            if credentials is None:
                job.failed += 1
                job.items.append({"email": name[:-len(ACCOUNT_FILE_SUFFIX)], "status": "failed", "reason": "凭据格式无法解析"})
                continue
            email, password, sso = credentials
            if not config.base_url:
                if save_local_copy(config, email, password, sso):
                    job.imported += 1
                    job.items.append({"email": email, "status": "created", "reason": ""})
                else:
                    job.failed += 1
                    job.items.append({"email": email, "status": "failed", "reason": "写入输出目录失败"})
                continue
            ok, reason = post_credentials(config, email, password, sso)
            if ok:
                job.imported += 1
                job.items.append({"email": email, "status": "imported", "reason": ""})
            else:
                job.failed += 1
                job.items.append({"email": email, "status": "failed", "reason": reason})
        if not fresh:
            job.update(last_error="注册机未产出新账号 (exit=%d): %s" % (code, tail[-400:]))
    except subprocess.TimeoutExpired:
        job.update(last_error="注册机执行超时")
    except Exception as exc:  # noqa: BLE001 - 任务级兜底
        log("任务异常: %s" % traceback.format_exc(limit=3))
        job.update(last_error="任务异常: %r" % (exc,))
    finally:
        if backup:
            restore_registrar_config(config.app_dir, backup)
        job.update(
            status="canceled" if job.canceled else ("done" if job.imported else "failed"),
            finished_at=time.time(),
        )


# --------------------------------------------------------------------------- HTTP


class Handler(BaseHTTPRequestHandler):
    server_version = "grok2api-account-registrar/1"
    protocol_version = "HTTP/1.1"

    store = JobStore()
    config: Config

    def log_message(self, fmt: str, *args) -> None:  # noqa: A003 - 基类签名
        return

    # ---- helpers

    def _authorized(self) -> bool:
        expected = self.config.internal_token
        if not expected:
            return False
        header = self.headers.get("Authorization", "")
        if not header.startswith("Bearer "):
            return False
        return header[len("Bearer "):].strip() == expected

    def _send(self, status: int, payload: dict) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status: int, code: str, message: str) -> None:
        self._send(status, {"error": {"code": code, "message": message}})

    def _read_json(self) -> dict:
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            return {}
        try:
            return json.loads(self.rfile.read(length).decode("utf-8"))
        except Exception:  # noqa: BLE001
            return {}

    # ---- routes

    def do_GET(self) -> None:  # noqa: N802 - 基类签名
        path = urlparse(self.path).path
        if path == "/healthz":
            self._send(200, {"data": {"ok": True, "enabled": self.config.enabled, "appDirReady": os.path.isdir(self.config.app_dir)}})
            return
        if not self._authorized():
            self._error(401, "unauthorized", "内部令牌无效")
            return
        if path.startswith("/jobs/"):
            job = self.store.get(path.rsplit("/", 1)[-1])
            if job is None:
                self._error(404, "jobNotFound", "任务不存在")
                return
            self._send(200, {"data": job.snapshot()})
            return
        self._error(404, "notFound", "未知路径")

    def do_POST(self) -> None:  # noqa: N802 - 基类签名
        path = urlparse(self.path).path
        if not self._authorized():
            self._error(401, "unauthorized", "内部令牌无效")
            return
        if not self.config.enabled:
            self._error(409, "registrarDisabled", "注册机 sidecar 未启用")
            return
        if path == "/jobs":
            payload = self._read_json()
            try:
                count = int(payload.get("count") or 1)
            except (TypeError, ValueError):
                self._error(400, "invalidRequest", "count 无效")
                return
            if count < 1 or count > self.config.max_count:
                self._error(400, "invalidRequest", "count 必须在 1..%d 之间" % self.config.max_count)
                return
            domains = [str(v).strip() for v in (payload.get("domains") or []) if str(v).strip()]
            domains = domains or list(self.config.domains)
            job = Job(count=count, domains=domains)
            self.store.add(job)
            threading.Thread(target=run_job, args=(job, self.config), daemon=True).start()
            self._send(202, {"data": job.snapshot()})
            return
        if path.startswith("/jobs/") and path.endswith("/cancel"):
            job = self.store.get(path.split("/")[-2])
            if job is None:
                self._error(404, "jobNotFound", "任务不存在")
                return
            job.canceled = True
            self._send(200, {"data": job.snapshot()})
            return
        self._error(404, "notFound", "未知路径")


def run_once() -> int:
    """一次性运行：注册完把结果打到 stdout / 挂载目录，不需要后端。"""
    config = Config.from_env()
    count = max(1, min(int(os.environ.get("REGISTRAR_COUNT", "1") or 1), MAX_JOB_ITEMS))
    config.max_count = max(config.max_count, count)
    job = Job(count=count, domains=list(config.domains))
    run_job(job, config)
    snapshot = job.snapshot()
    snapshot["appDir"] = config.app_dir
    snapshot["domains"] = job.domains
    print(json.dumps(snapshot, ensure_ascii=False, indent=2), flush=True)
    return 0 if job.imported > 0 else 1


def main() -> int:
    if "--once" in sys.argv:
        return run_once()
    config = load_config()
    if not config.internal_token:
        log("bootstrap 缺少 internal_token，拒绝启动")
        return 2
    Handler.config = config
    host = os.environ.get("REGISTRAR_LISTEN_HOST", "0.0.0.0")
    port = int(os.environ.get("REGISTRAR_LISTEN_PORT", "8099"))
    server = ThreadingHTTPServer((host, port), Handler)
    log("listening on %s:%d enabled=%s appDir=%s" % (host, port, config.enabled, config.app_dir))
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
