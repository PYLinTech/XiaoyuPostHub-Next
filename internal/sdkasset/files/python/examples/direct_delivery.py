#!/usr/bin/env python3
"""后端请求得到地址后，前端直接去拿的示例。

浏览器只向本示例后端要一份交付计划，再拿计划里的 123 地址直接取密文，
并在浏览器本地解密；API Key 只保存在本进程，不会返回给浏览器。

注意「后端 IP ≠ 访客 IP」：站点看到的是本后端的出口 IP，而真正去 123 取
字节的是浏览器用户。票据默认按申请方网段绑定，若不代传访客 IP，真实部署
（后端在服务器、用户在别处）直连必然 403。本示例把观测到的访客地址通过
client_ip 传给站点，票据据此绑定。若后端也在反向代理之后，用 TRUSTED_PROXIES
（逗号分隔的 IP 或 CIDR，默认空）声明代理网段，再从 X-Forwarded-For 右侧跳过
受信代理取真实访客；站点侧「可信代理」只作用于站点自身的入站请求，不会替本
后端判断转发头。
"""
from __future__ import annotations

import ipaddress
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

SDK_DIR = Path(__file__).resolve().parents[1]
PAGE = Path(__file__).resolve().parents[2] / "web" / "direct-delivery.html"
MODULE = Path(__file__).resolve().parents[2] / "web" / "browser-delivery.mjs"
sys.path.insert(0, str(SDK_DIR))

from xiaoyuposthub_next_sdk_v5 import Client  # noqa: E402

PORT = int(os.environ.get("PORT", "8080"))
MAX_BODY = 1 << 20
client: Client


def _normalize_ip(raw: object) -> str:
    value = str(raw or "").strip()
    if not value:
        return ""
    if value.startswith("["):
        end = value.find("]")
        if end > 0:
            value = value[1:end]
    elif value.count(":") == 1 and "." in value:
        value = value.split(":", 1)[0]
    try:
        addr = ipaddress.ip_address(value)
    except ValueError:
        return ""
    if isinstance(addr, ipaddress.IPv6Address) and addr.ipv4_mapped:
        addr = addr.ipv4_mapped
    return str(addr)


def _parse_trusted_proxies(
    raw: str,
) -> tuple[ipaddress.IPv4Network | ipaddress.IPv6Network, ...]:
    networks: list[ipaddress.IPv4Network | ipaddress.IPv6Network] = []
    for item in raw.split(","):
        item = item.strip()
        if not item:
            continue
        try:
            networks.append(ipaddress.ip_network(item, strict=False))
            continue
        except ValueError:
            addr = _normalize_ip(item)
            if addr:
                networks.append(ipaddress.ip_network(addr))
    return tuple(networks)


TRUSTED_PROXIES = _parse_trusted_proxies(os.environ.get("TRUSTED_PROXIES", ""))


def _is_trusted_proxy(addr: str) -> bool:
    if not addr:
        return False
    ip = ipaddress.ip_address(addr)
    return any(ip in network for network in TRUSTED_PROXIES)


def _visitor_ip(handler: BaseHTTPRequestHandler) -> str:
    """只有 TCP 对端是受信代理时才读取 X-Forwarded-For，并从右往左跳过
    受信代理。转发链里出现无法解析的条目时回退到 TCP 对端，不继续相信
    更左侧、可能由客户端伪造的值。
    """
    peer = _normalize_ip(handler.client_address[0])
    if not peer:
        return ""
    if not _is_trusted_proxy(peer):
        return peer
    hops = handler.headers.get("X-Forwarded-For", "").split(",")
    leftmost = ""
    for hop in reversed(hops):
        addr = _normalize_ip(hop)
        if not addr:
            return peer
        leftmost = addr
        if not _is_trusted_proxy(addr):
            return addr
    return leftmost or peer


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _json(self, status: int, value: dict) -> None:
        raw = json.dumps(value, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(raw)

    def _file(self, path: Path, content_type: str) -> None:
        raw = path.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(raw)

    def _body(self) -> dict:
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > MAX_BODY:
            raise ValueError("请求体为空或过大")
        return json.loads(self.rfile.read(length) or b"{}")

    def do_GET(self) -> None:  # noqa: N802
        route = self.path.split("?", 1)[0]
        if route == "/":
            self._file(PAGE, "text/html; charset=utf-8")
            return
        if route == "/browser-delivery.mjs":
            self._file(MODULE, "text/javascript; charset=utf-8")
            return
        self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        route = self.path.split("?", 1)[0]
        if route not in ("/api/delivery/plan", "/api/delivery/settle"):
            self._json(404, {"error": "not found"})
            return
        try:
            body = self._body()
        except (ValueError, json.JSONDecodeError) as exc:
            self._json(400, {"error": str(exc) or "请求体不是有效 JSON"})
            return

        if route == "/api/delivery/settle":
            ticket_id = str(body.get("ticketId") or "").strip()
            if not ticket_id:
                self._json(400, {"error": "ticketId 不能为空"})
                return
            try:
                client.settle(ticket_id, bool(body.get("completed")))
            except Exception as exc:  # noqa: BLE001 - 示例统一把上游失败返回 502
                print(f"方式 2 示例失败: {exc}", file=sys.stderr)
                self._json(502, {"error": str(exc) or "请求失败"})
                return
            self._json(200, {"ok": True})
            return

        file_path = str(body.get("path") or "").strip()
        client_public_key = str(body.get("clientPublicKey") or "").strip()
        if not file_path:
            self._json(400, {"error": "path 不能为空"})
            return
        if not client_public_key:
            self._json(400, {"error": "clientPublicKey 不能为空"})
            return

        try:
            # 代传终端用户真实地址：直连交付的票据按它绑定网段，123 回源才放行。
            plan = client.prepare_download(
                file_path, client_public_key, client_ip=_visitor_ip(self))
            if plan.get("mode") != "direct" or plan.get("contentForm") != "ciphertext":
                # 不回执：密文票据一旦回执即按预扣全额结算，不回执则到期退款；
                # proxy_decrypt 由服务端结算，回执也会被拒绝。
                self._json(409, {
                    "error": "本次交付被降级为站点中转，浏览器无法直接向 123 取密文；"
                             "请确认站点开启「优先直链交付」且 123 直链空间可用",
                })
                return
        except Exception as exc:  # noqa: BLE001 - 示例统一把上游失败返回 502
            print(f"方式 2 示例失败: {exc}", file=sys.stderr)
            self._json(502, {"error": str(exc) or "请求失败"})
            return

        self._json(200, plan)

    def log_message(self, fmt: str, *args: object) -> None:
        print(f"方式 2 示例: {fmt % args}", file=sys.stderr)


def main() -> None:
    global client
    client = Client()
    server = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"方式 2 示例（后端请求得到地址后，前端直接去拿）已启动：http://127.0.0.1:{PORT}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
