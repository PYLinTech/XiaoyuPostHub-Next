#!/usr/bin/env python3
"""XiaoyuPostHub-Next Python SDK 示例。

站点地址只来自 ../config.json；API Key 只来自参数或 XIAOYU_API_KEY。
下载走客户端本地 XPH 解密，明文不会经过服务端额外出口。
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
import shutil
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding, rsa
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

CONFIG_PATH = Path(__file__).resolve().parent.parent / "config.json"


class APIError(Exception):
    def __init__(self, status: int, message: str, detail: str = ""):
        super().__init__(message)
        self.status = status
        self.message = message
        self.detail = detail


def load_base_url() -> str:
    with CONFIG_PATH.open("r", encoding="utf-8") as fh:
        return str(json.load(fh)["baseUrl"]).rstrip("/")


def _open_url(url: str, headers: dict[str, str] | None = None) -> urllib.request.urlopen:
    """只允许 http(s)：交付计划里的地址来自服务端，但配置文件也可能被改坏。"""
    scheme = urllib.parse.urlsplit(url).scheme.lower()
    if scheme not in ("http", "https"):
        raise ValueError(f"不支持的下载地址协议：{scheme or '空'}")
    return urllib.request.urlopen(urllib.request.Request(url, headers=headers or {}))


class Client:
    def __init__(self, api_key: str = "", base_url: str = ""):
        self.api_key = api_key or os.environ.get("XIAOYU_API_KEY", "")
        if not self.api_key:
            raise ValueError("缺少 API Key：设置 XIAOYU_API_KEY 或传入 api_key")
        self.base_url = (base_url or load_base_url()).rstrip("/")
        if urllib.parse.urlsplit(self.base_url).scheme.lower() not in ("http", "https"):
            raise ValueError("站点地址必须以 http:// 或 https:// 开头")

    # ---------------------------------------------------------------- HTTP
    def _open(self, method: str, path: str, data: bytes | None = None,
              headers: dict[str, str] | None = None) -> urllib.request.urlopen:
        merged = {"Authorization": f"Bearer {self.api_key}"}
        merged.update(headers or {})
        req = urllib.request.Request(self.base_url + path, data=data, method=method, headers=merged)
        try:
            return urllib.request.urlopen(req)
        except urllib.error.HTTPError as exc:
            raw = exc.read().decode("utf-8", "replace")
            message, detail = f"HTTP {exc.code}", raw[:500]
            try:
                err = json.loads(raw).get("error") or {}
                message = err.get("message") or message
                detail = err.get("detail") or detail
            except Exception:
                pass
            raise APIError(exc.code, message, detail) from None

    def request(self, method: str, path: str, body: dict | None = None,
                raw: bytes | None = None) -> dict:
        data, headers = None, {}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        elif raw is not None:
            data = raw
            headers["Content-Type"] = "application/octet-stream"
        with self._open(method, path, data, headers) as resp:
            payload = json.loads(resp.read() or b"{}")
        if payload.get("error"):
            err = payload["error"]
            raise APIError(0, err.get("message", "请求失败"), err.get("detail", ""))
        return payload.get("data") or {}

    def _fetch(self, url: str, dest: str, auth: bool = False) -> None:
        headers = {"Authorization": f"Bearer {self.api_key}"} if auth else {}
        with _open_url(url, headers) as src, open(dest, "wb") as out:
            shutil.copyfileobj(src, out)

    # ---------------------------------------------------------------- 文件树
    def profile(self) -> dict:
        return self.request("GET", "/api/auth/profile")

    def list(self, path: str = "/") -> dict:
        return self.request("GET", "/api/fs/list?" + urllib.parse.urlencode({"path": path}))

    def stat(self, path: str) -> dict:
        return self.request("GET", "/api/fs/stat?" + urllib.parse.urlencode({"path": path}))

    def stats(self, path: str = "/") -> dict:
        return self.request("GET", "/api/fs/stats?" + urllib.parse.urlencode({"path": path}))

    def mkdir(self, parent: str, name: str) -> dict:
        return self.request("POST", "/api/fs/mkdir", {"parentPath": parent, "name": name})

    def rename(self, path: str, new_name: str) -> dict:
        return self.request("POST", "/api/fs/rename", {"path": path, "newName": new_name})

    def move(self, path: str, dest_parent: str) -> dict:
        return self.request("POST", "/api/fs/move", {"path": path, "destParent": dest_parent})

    def remove(self, path: str) -> dict:
        return self.request("POST", "/api/fs/delete", {"path": path})

    def delete(self, path: str) -> dict:
        return self.remove(path)

    # ---------------------------------------------------------------- 上传
    def upload(self, local_path: str, parent: str = "/", name: str = "",
               conflict_action: str = "rename", poll_seconds: float = 1.0) -> dict:
        src = Path(local_path)
        size = src.stat().st_size
        digest = hashlib.sha256()
        with src.open("rb") as fh:
            for chunk in iter(lambda: fh.read(4 << 20), b""):
                digest.update(chunk)
        init = self.request("POST", "/api/upload/init", {
            "checksum": digest.hexdigest(),
            "sizePlain": size,
            "parentPath": parent,
            "name": name or src.name,
            "conflictAction": conflict_action,
        })
        if init.get("dedup"):
            return init
        session = init["sessionId"]
        chunk_size = int(init["chunkSize"])
        received = set(init.get("received") or [])
        with src.open("rb") as fh:
            for index in range(int(init["chunkTotal"])):
                if index in received:
                    continue
                fh.seek(index * chunk_size)
                data = fh.read(chunk_size)
                self.request("PUT", f"/api/upload/{session}/chunk/{index}", raw=data)
        self.request("POST", f"/api/upload/{session}/complete")
        deadline = time.time() + 300
        while time.time() < deadline:
            status = self.request("GET", f"/api/upload/{session}")
            state = status.get("state")
            if state == "done":
                return status
            if state == "error":
                raise APIError(0, "上传失败", status.get("error", ""))
            time.sleep(poll_seconds)
        raise APIError(0, "上传收尾超时", session)

    def cancel_upload(self, session: str) -> dict:
        return self.request("POST", f"/api/upload/{session}/cancel")

    # ---------------------------------------------------------------- 下载
    def download(self, path: str, dest: str, preview: bool = False) -> dict:
        private_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        pub = private_key.public_key().public_bytes(
            serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
        plan = self.request("POST", "/api/fs/preview" if preview else "/api/fs/download", {
            "path": path,
            "clientPublicKey": base64.b64encode(pub).decode("ascii"),
        })
        mode, meta = plan.get("mode"), plan.get("encryption") or {}
        if plan.get("contentForm") == "plaintext":
            url = plan.get("streamUrl")
            if not url:
                raise APIError(0, "交付计划缺少可下载地址", "")
            self._fetch(self.base_url + url, dest, auth=True)
            return plan

        temp = tempfile.NamedTemporaryFile(prefix="xph-", delete=False)
        temp_path = temp.name
        temp.close()
        try:
            parts = plan.get("parts") or []
            if mode == "direct" and parts:
                with open(temp_path, "wb") as out:
                    for part in parts:
                        with _open_url(part["url"]) as src:
                            shutil.copyfileobj(src, out)
            elif mode == "direct" and plan.get("url"):
                self._fetch(plan["url"], temp_path)
            elif plan.get("streamUrl"):
                self._fetch(self.base_url + plan["streamUrl"], temp_path, auth=True)
            else:
                raise APIError(0, "交付计划缺少可下载地址", "")
            self._decrypt_file(temp_path, dest, meta, private_key)
        finally:
            Path(temp_path).unlink(missing_ok=True)

        ticket = plan.get("ticketId")
        if ticket:
            try:
                self.request("POST", "/api/fs/settle", {"ticketId": ticket, "completed": True})
            except APIError:
                pass
        return plan

    def _decrypt_file(self, cipher_path: str, dest: str, meta: dict, private_key) -> None:
        with open(cipher_path, "rb") as src:
            header = src.read(64)
            if len(header) < 64 or header[:8] != b"XPHCRPT1":
                raise APIError(0, "密文文件头无效", "")
            block = 1 << header[9]
            dek = private_key.decrypt(
                base64.b64decode(meta["keyEnvelope"]),
                padding.OAEP(mgf=padding.MGF1(algorithm=hashes.SHA256()),
                             algorithm=hashes.SHA256(), label=None),
            )
            aes = AESGCM(dek)
            aad = header[:40]
            prefix = int(meta["noncePrefix"]).to_bytes(4, "big")
            remaining = int(meta["plainSize"])
            digest = hashlib.sha256()
            index = 0
            with open(dest, "wb") as out:
                while remaining > 0:
                    take = min(block, remaining)
                    encrypted = src.read(take + 16)
                    if len(encrypted) != take + 16:
                        raise APIError(0, "密文被截断", "")
                    nonce = prefix + index.to_bytes(8, "big")
                    plain = aes.decrypt(nonce, encrypted, aad)
                    out.write(plain)
                    digest.update(plain)
                    remaining -= take
                    index += 1
        if meta.get("checksum") and digest.hexdigest() != meta["checksum"]:
            Path(dest).unlink(missing_ok=True)
            raise APIError(0, "下载内容校验失败", "SHA-256 与交付计划不一致")

    # ---------------------------------------------------------------- 分享
    def create_share(self, path: str, kind: str = "file", access_mode: str = "public",
                     password: str = "", expires_at: int = 0, allow_download: bool = True,
                     allow_preview: bool = True) -> dict:
        return self.request("POST", "/api/share", {
            "path": path, "kind": kind, "accessMode": access_mode,
            "password": password, "expiresAt": expires_at,
            "allowDownload": allow_download, "allowPreview": allow_preview,
        })["share"]

    def list_shares(self, limit: int = 100, offset: int = 0) -> dict:
        return self.request("GET", f"/api/share?limit={limit}&offset={offset}")

    def delete_share(self, share_id: str) -> dict:
        return self.request("DELETE", f"/api/share/{share_id}")

    def share_url(self, share_id: str) -> str:
        return f"{self.base_url}/s/{share_id}"

    # ---------------------------------------------------------------- 直链
    def create_direct_link(self, path: str, expires_at: int = 0, max_hits: int = 0) -> dict:
        return self.request("POST", "/api/direct-link", {
            "path": path, "expiresAt": expires_at, "maxHits": max_hits,
        })["link"]

    def list_direct_links(self, limit: int = 100, offset: int = 0) -> dict:
        return self.request("GET", f"/api/direct-link?limit={limit}&offset={offset}")

    def delete_direct_link(self, link_id: str) -> dict:
        return self.request("DELETE", f"/api/direct-link/{link_id}")

    def direct_link_url(self, link_id: str) -> str:
        return f"{self.base_url}/d/{link_id}"


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("用法: python xiaoyu.py <本地文件> [目标目录]")
        raise SystemExit(2)
    client = Client()
    local, parent = sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "/"
    result = client.upload(local, parent)
    print(json.dumps(result, ensure_ascii=False, indent=2))
