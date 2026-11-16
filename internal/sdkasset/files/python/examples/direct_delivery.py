#!/usr/bin/env python3
"""后端请求得到地址后，前端直接去拿的示例。

浏览器只向本示例后端要一份交付计划，再拿计划里的 123 地址直接取密文，
并在浏览器本地解密；API Key 只保存在本进程，不会返回给浏览器。
"""
from __future__ import annotations

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

SDK_DIR = Path(__file__).resolve().parents[1]
PAGE = Path(__file__).resolve().parents[2] / "web" / "direct-delivery.html"
MODULE = Path(__file__).resolve().parents[2] / "web" / "browser-delivery.mjs"
sys.path.insert(0, str(SDK_DIR))

from xiaoyuposthub_next_sdk_v4 import Client  # noqa: E402

PORT = int(os.environ.get("PORT", "8080"))
MAX_BODY = 1 << 20
client: Client


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
            plan = client.prepare_download(file_path, client_public_key)
            if plan.get("mode") != "direct" or plan.get("contentForm") != "ciphertext":
                # 密文票据由客户端回执；proxy_decrypt 由服务端在开流时自行结算，回执会被拒绝。
                if plan.get("contentForm") == "ciphertext":
                    try:
                        client.settle(plan.get("ticketId", ""), False)
                    except Exception:  # noqa: BLE001 - 回执失败不影响降级响应
                        pass
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
