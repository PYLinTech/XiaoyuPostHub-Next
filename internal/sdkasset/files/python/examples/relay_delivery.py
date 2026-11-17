#!/usr/bin/env python3
"""后端直连，由后端做中转的示例。

本示例后端用 SDK 取回文件，密文计划在本地解密，再把明文字节转发给浏览器；API Key 不出后端。
示例使用临时文件承载解密结果，生产环境可按需改为缓存、对象存储或流式实现。
"""
from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import quote

SDK_DIR = Path(__file__).resolve().parents[1]
PAGE = Path(__file__).resolve().parents[2] / "web" / "relay-delivery.html"
sys.path.insert(0, str(SDK_DIR))

from xiaoyuposthub_next_sdk_v5 import Client  # noqa: E402

PORT = int(os.environ.get("PORT", "8081"))
MAX_BODY = 1 << 20
client: Client


def content_disposition(name: str) -> str:
    fallback = "".join(ch if 0x20 <= ord(ch) < 0x7F and ch not in '"\\' else "_" for ch in name)
    return f"attachment; filename=\"{fallback}\"; filename*=UTF-8''{quote(name, safe='')}"


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
        if self.path.split("?", 1)[0] != "/api/delivery/relay":
            self._json(404, {"error": "not found"})
            return
        try:
            body = self._body()
            file_path = str(body.get("path") or "").strip()
            if not file_path:
                raise ValueError("path 不能为空")
        except (ValueError, json.JSONDecodeError) as exc:
            self._json(400, {"error": str(exc) or "请求体不是有效 JSON"})
            return

        fd, temp_path = tempfile.mkstemp(prefix="xph-relay-")
        os.close(fd)
        started = False
        try:
            try:
                plan = client.download(file_path, temp_path)
            except Exception as exc:  # noqa: BLE001 - 示例统一把上游失败返回 502
                print(f"方式 1 示例失败: {exc}", file=sys.stderr)
                self._json(502, {"error": str(exc) or "下载失败"})
                return

            name = plan.get("fileName") or Path(file_path).name or "download.bin"
            self.send_response(200)
            started = True
            self.send_header("Content-Type", plan.get("mimeType") or "application/octet-stream")
            self.send_header("Content-Length", str(os.path.getsize(temp_path)))
            self.send_header("Content-Disposition", content_disposition(name))
            self.send_header("Cache-Control", "private, no-store")
            self.end_headers()
            with open(temp_path, "rb") as src:
                shutil.copyfileobj(src, self.wfile, length=1 << 20)
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as exc:  # noqa: BLE001 - 响应已开始写出时只能记录
            print(f"方式 1 示例失败: {exc}", file=sys.stderr)
            if not started:
                self._json(502, {"error": str(exc) or "请求失败"})
        finally:
            try:
                os.unlink(temp_path)
            except FileNotFoundError:
                pass

    def log_message(self, fmt: str, *args: object) -> None:
        print(f"方式 1 示例: {fmt % args}", file=sys.stderr)


def main() -> None:
    global client
    client = Client()
    server = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"方式 1 示例（后端直连，由后端做中转）已启动：http://127.0.0.1:{PORT}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
