#!/usr/bin/env python3
"""后端接入 + 前端直连交付示例。

浏览器只向本示例后端要一条直链，文件字节由浏览器直接向 XiaoyuPostHub 请求。
API Key 只保存在本进程，不会返回给浏览器。
"""
from __future__ import annotations

import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

SDK_DIR = Path(__file__).resolve().parents[1]
PAGE = Path(__file__).resolve().parents[2] / "web" / "direct-delivery.html"
sys.path.insert(0, str(SDK_DIR))

from xiaoyuposthub_next_sdk_v3 import Client  # noqa: E402

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

    def _body(self) -> dict:
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > MAX_BODY:
            raise ValueError("请求体为空或过大")
        return json.loads(self.rfile.read(length) or b"{}")

    def do_GET(self) -> None:  # noqa: N802
        if self.path.split("?", 1)[0] != "/":
            self._json(404, {"error": "not found"})
            return
        raw = PAGE.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self) -> None:  # noqa: N802
        if self.path.split("?", 1)[0] != "/api/delivery/direct":
            self._json(404, {"error": "not found"})
            return
        try:
            body = self._body()
            file_path = str(body.get("path") or "").strip()
            if not file_path:
                raise ValueError("path 不能为空")
            expires_at = int(body.get("expiresAt") or 0) or int(time.time()) + 3600
            max_hits = int(body.get("maxHits") or 0)
        except (ValueError, json.JSONDecodeError) as exc:
            self._json(400, {"error": str(exc) or "请求体不是有效 JSON"})
            return

        try:
            link = client.create_direct_link(file_path, expires_at=expires_at, max_hits=max_hits)
        except Exception as exc:  # noqa: BLE001 - 示例统一把上游失败返回 502
            print(f"直连交付示例失败: {exc}", file=sys.stderr)
            self._json(502, {"error": str(exc) or "请求失败"})
            return

        self._json(200, {
            "url": client.direct_link_url(link["id"]),
            "fileName": link.get("sourceName", ""),
            "size": link.get("size", 0),
            "expiresAt": link.get("expiresAt", 0),
        })

    def log_message(self, fmt: str, *args: object) -> None:
        print(f"直连交付示例: {fmt % args}", file=sys.stderr)


def main() -> None:
    global client
    client = Client()
    server = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"直连交付示例已启动：http://127.0.0.1:{PORT}（浏览器会直接向站点取文件）")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
