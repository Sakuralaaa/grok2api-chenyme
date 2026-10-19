#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""本地免认证 SOCKS5 转发 -> 上游带认证 SOCKS5。

为什么需要它：
  Playwright / Camoufox 明确拒绝带用户名密码的 socks5
  （BrowserType.launch: Browser does not support socks5 proxy authentication），
  而市面上带认证的节点几乎都是 socks5。这里在本机开一个免认证入口，
  把请求转发到上游认证节点，浏览器就能正常启动，出口 IP 仍是上游节点。

另外上游若只接受「域名型」请求（部分 WARP 落地会直接断开 IPv4 ATYP 的
CONNECT），这个转发统一用域名形式发送，也能绕过该限制。

环境变量：
  RELAY_LISTEN_HOST   默认 127.0.0.1
  RELAY_LISTEN_PORT   默认 10800
  RELAY_UPSTREAM      上游 socks5 URL，例 socks5://user:pass@host:port
"""
from __future__ import annotations

import os
import select
import socket
import struct
import sys
import threading
from urllib.parse import unquote, urlparse


def log(message: str) -> None:
    sys.stderr.write("[relay] %s\n" % message)
    sys.stderr.flush()


def recvn(sock: socket.socket, size: int) -> bytes:
    buf = b""
    while len(buf) < size:
        chunk = sock.recv(size - len(buf))
        if not chunk:
            raise OSError("peer closed")
        buf += chunk
    return buf


class Upstream:
    def __init__(self, url: str) -> None:
        parsed = urlparse(url)
        if parsed.scheme not in ("socks5", "socks5h"):
            raise ValueError("RELAY_UPSTREAM 只支持 socks5/socks5h: %s" % parsed.scheme)
        if not parsed.hostname:
            raise ValueError("RELAY_UPSTREAM 缺少主机名")
        self.host = parsed.hostname
        self.port = parsed.port or 1080
        self.username = unquote(parsed.username or "").encode("utf-8")
        self.password = unquote(parsed.password or "").encode("utf-8")


class Relay:
    def __init__(self, upstream: Upstream, host: str, port: int) -> None:
        self.upstream = upstream
        self.host = host
        self.port = port

    def connect_upstream(self, host: str, port: int) -> socket.socket:
        up = socket.create_connection((self.upstream.host, self.upstream.port), timeout=25)
        up.settimeout(25)
        if self.upstream.username:
            up.sendall(b"\x05\x02\x00\x02")
        else:
            up.sendall(b"\x05\x01\x00")
        _, method = recvn(up, 2)
        if method == 2:
            user, pwd = self.upstream.username, self.upstream.password
            if len(user) > 255 or len(pwd) > 255:
                up.close()
                raise OSError("上游账号密码过长")
            up.sendall(b"\x01" + bytes([len(user)]) + user + bytes([len(pwd)]) + pwd)
            _, status = recvn(up, 2)
            if status != 0:
                up.close()
                raise OSError("上游认证失败 status=%d" % status)
        elif method != 0:
            up.close()
            raise OSError("上游不接受可用的认证方式 method=%d" % method)

        # 统一以域名形式下发，规避只接受域名型请求的落地节点
        name = host.encode("idna") if isinstance(host, str) else host
        if len(name) > 255:
            up.close()
            raise OSError("主机名过长")
        up.sendall(b"\x05\x01\x00\x03" + bytes([len(name)]) + name + struct.pack("!H", port))
        _, rep, _, atyp = recvn(up, 4)
        if atyp == 1:
            recvn(up, 4)
        elif atyp == 3:
            recvn(up, recvn(up, 1)[0])
        elif atyp == 4:
            recvn(up, 16)
        recvn(up, 2)
        if rep != 0:
            up.close()
            raise OSError("上游 CONNECT 失败 rep=%d host=%s" % (rep, host))
        up.settimeout(None)
        return up

    @staticmethod
    def pipe(a: socket.socket, b: socket.socket) -> None:
        try:
            while True:
                readable, _, errors = select.select([a, b], [], [a, b], 300)
                if errors or not readable:
                    return
                for sock in readable:
                    data = sock.recv(65536)
                    if not data:
                        return
                    (b if sock is a else a).sendall(data)
        except OSError:
            return

    def handle(self, client: socket.socket) -> None:
        up = None
        try:
            client.settimeout(30)
            _, count = recvn(client, 2)
            recvn(client, count)
            client.sendall(b"\x05\x00")
            _, cmd, _, atyp = recvn(client, 4)
            if cmd != 1:
                client.sendall(b"\x05\x07\x00\x01" + b"\x00" * 6)
                return
            if atyp == 1:
                host = socket.inet_ntoa(recvn(client, 4))
            elif atyp == 3:
                host = recvn(client, recvn(client, 1)[0]).decode("utf-8", "ignore")
            else:
                host = socket.inet_ntop(socket.AF_INET6, recvn(client, 16))
            port = struct.unpack("!H", recvn(client, 2))[0]

            up = self.connect_upstream(host, port)
            client.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
            client.settimeout(None)
            self.pipe(client, up)
        except OSError as exc:
            log("转发失败: %r" % (exc,))
        finally:
            for sock in (up, client):
                try:
                    if sock is not None:
                        sock.close()
                except OSError:
                    pass

    def serve_forever(self) -> None:
        server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        server.bind((self.host, self.port))
        server.listen(200)
        log("listening %s:%d -> %s:%d" % (self.host, self.port, self.upstream.host, self.upstream.port))
        while True:
            client, _ = server.accept()
            threading.Thread(target=self.handle, args=(client,), daemon=True).start()


def main() -> int:
    raw = os.environ.get("RELAY_UPSTREAM", "").strip()
    if not raw:
        log("未配置 RELAY_UPSTREAM，转发不启动")
        return 0
    relay = Relay(
        Upstream(raw),
        os.environ.get("RELAY_LISTEN_HOST", "127.0.0.1"),
        int(os.environ.get("RELAY_LISTEN_PORT", "10800")),
    )
    relay.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
