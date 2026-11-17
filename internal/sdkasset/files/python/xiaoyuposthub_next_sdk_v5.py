#!/usr/bin/env python3
"""XiaoyuPostHub-Next Python SDK 示例。

站点地址默认从 ../config.json 读取，可在 Client 的 base_url 参数覆盖；
API Key 只来自参数或 XIAOYU_API_KEY。
版本查询是公开接口，不需要 API Key；其余请求都需要 Key。
所有 path 参数都相对于 Key 的工作区，"/" 就是工作区根目录。
只做存储集成时直接使用 upload/download，无需创建分享或直链。
密文计划在客户端本地做 XPH 解密并核对文件头与交付记录；
明文计划同样核对长度和 SHA-256，校验通过后才替换目标文件。
覆盖文件树、上传任务、下载/预览、分享与取件码、直链、归档和公告等用户侧操作。
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding, rsa
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

CONFIG_PATH = Path(__file__).resolve().parent.parent / "config.json"


class APIError(Exception):
    def __init__(self, status: int, message: str, detail: str = "", retry_after: int | None = None):
        super().__init__(message)
        self.status = status
        self.message = message
        self.detail = detail
        self.retry_after = retry_after


def load_base_url() -> str:
    with CONFIG_PATH.open("r", encoding="utf-8") as fh:
        return str(json.load(fh)["baseUrl"]).rstrip("/")


def _normalize_base_url(raw: str) -> str:
    base = raw.rstrip("/")
    parsed = urllib.parse.urlsplit(base)
    if parsed.scheme.lower() not in ("http", "https") or not parsed.netloc:
        raise ValueError("站点地址必须以 http:// 或 https:// 开头")
    return base


def _path(value: str) -> str:
    return urllib.parse.quote(str(value), safe="")


def _open_url(url: str, headers: dict[str, str] | None = None) -> Any:
    """只允许 http(s)：交付计划里的地址来自服务端，但配置文件也可能被改坏。"""
    scheme = urllib.parse.urlsplit(url).scheme.lower()
    if scheme not in ("http", "https"):
        raise ValueError(f"不支持的地址协议：{scheme or '空'}")
    return urllib.request.urlopen(urllib.request.Request(url, headers=headers or {}))


def _retry_after(headers) -> int | None:
    raw = headers.get("Retry-After") if headers else None
    if not raw:
        return None
    try:
        seconds = int(raw)
    except ValueError:
        return None
    return seconds if seconds > 0 else None


def _http_error(exc: urllib.error.HTTPError) -> APIError:
    raw = exc.read().decode("utf-8", "replace")
    message, detail = f"HTTP {exc.code}", raw[:500]
    try:
        err = json.loads(raw).get("error") or {}
        message = err.get("message") or message
        detail = err.get("detail") or detail
    except Exception:
        pass
    return APIError(exc.code, message, detail, _retry_after(exc.headers))


def _network_error(exc: urllib.error.URLError) -> APIError:
    return APIError(0, "网络连接失败，请检查网络后重试", str(exc.reason))


def sdk_version(base_url: str = "") -> str:
    """查询站点公开的示例 SDK 版本；不需要 API Key。"""
    base = _normalize_base_url(base_url or load_base_url())
    try:
        with _open_url(base + "/api/sdk/version") as resp:
            payload = json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as exc:
        raise _http_error(exc) from None
    except urllib.error.URLError as exc:
        raise _network_error(exc) from None
    if payload.get("error"):
        err = payload["error"]
        raise APIError(0, err.get("message", "请求失败"), err.get("detail", ""))
    return str((payload.get("data") or {}).get("version", ""))


class Client:
    UPLOAD_WORKERS = 3
    PART_WORKERS = 4

    def __init__(self, api_key: str = "", base_url: str = ""):
        self.api_key = api_key or os.environ.get("XIAOYU_API_KEY", "")
        if not self.api_key:
            raise ValueError("缺少 API Key：设置 XIAOYU_API_KEY 或传入 api_key")
        self.base_url = _normalize_base_url(base_url or load_base_url())

    # ---------------------------------------------------------------- 版本
    def sdk_version(self) -> str:
        """返回站点当前的示例 SDK 版本；该公开接口不发送 API Key。"""
        return sdk_version(self.base_url)

    # ---------------------------------------------------------------- HTTP
    def _open(self, method: str, path: str, data: bytes | None = None,
              headers: dict[str, str] | None = None) -> Any:
        merged = {"Authorization": f"Bearer {self.api_key}"}
        merged.update(headers or {})
        req = urllib.request.Request(self.base_url + path, data=data, method=method, headers=merged)
        try:
            return urllib.request.urlopen(req)
        except urllib.error.HTTPError as exc:
            raise _http_error(exc) from None
        except urllib.error.URLError as exc:
            raise _network_error(exc) from None

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

    def _resolve_url(self, raw: str) -> str:
        return self.base_url + raw if raw.startswith("/") else raw

    def _site_url(self, raw: str) -> str:
        """认证请求只能发往站内相对路径，避免把 API Key 交给计划里的外部地址。"""
        if not raw.startswith("/"):
            raise APIError(0, "交付计划的中转地址不正确", "")
        return self.base_url + raw

    def _copy_url(self, url: str, out, auth: bool = False) -> int:
        headers = {"Authorization": f"Bearer {self.api_key}"} if auth else {}
        copied = 0
        try:
            with _open_url(self._resolve_url(url), headers) as src:
                while True:
                    chunk = src.read(1 << 20)
                    if not chunk:
                        break
                    out.write(chunk)
                    copied += len(chunk)
        except urllib.error.HTTPError as exc:
            raise _http_error(exc) from None
        except urllib.error.URLError as exc:
            raise _network_error(exc) from None
        return copied

    def _fetch(self, url: str, dest: str, auth: bool = False) -> None:
        with open(dest, "wb") as out:
            self._copy_url(url, out, auth)

    def _write_part(self, url: str, path: str, offset: int, size: int) -> None:
        with open(path, "r+b") as out:
            out.seek(offset)
            copied = self._copy_url(url, out)
        if copied != size:
            raise APIError(
                0, "分卷长度不一致", f"偏移={offset} 期望={size} 实得={copied}")

    def _download_parts(self, parts: list[dict], dest: str) -> None:
        with open(dest, "wb") as out:
            out.truncate(sum(part["size"] for part in parts))
        with ThreadPoolExecutor(max_workers=min(self.PART_WORKERS, len(parts))) as pool:
            futures = [
                pool.submit(self._write_part, part["url"], dest,
                            part["offset"], part["size"])
                for part in parts
            ]
            try:
                for future in as_completed(futures):
                    future.result()
            except Exception:
                for future in futures:
                    future.cancel()
                raise

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
        session = _path(init["sessionId"])
        chunk_size = int(init["chunkSize"])
        received = set(init.get("received") or [])
        pending = [index for index in range(int(init["chunkTotal"])) if index not in received]

        def send_chunk(index: int) -> None:
            offset = index * chunk_size
            length = min(chunk_size, size - offset)
            if length <= 0:
                raise APIError(0, "读取本地文件失败：文件在上传过程中被截断", "")
            with src.open("rb") as fh:
                fh.seek(offset)
                data = fh.read(length)
            if len(data) != length:
                raise APIError(
                    0, "读取本地文件失败", f"偏移 {offset} 期望 {length} 字节，实得 {len(data)} 字节")
            self.request("PUT", f"/api/upload/{session}/chunk/{index}", raw=data)

        if pending:
            with ThreadPoolExecutor(max_workers=min(self.UPLOAD_WORKERS, len(pending))) as pool:
                futures = [pool.submit(send_chunk, index) for index in pending]
                try:
                    for future in as_completed(futures):
                        future.result()
                except Exception:
                    for future in futures:
                        future.cancel()
                    raise
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

    def upload_tasks(self) -> dict:
        return self.request("GET", "/api/upload/tasks")

    def upload_status(self, session: str) -> dict:
        return self.request("GET", f"/api/upload/{_path(session)}")

    def cancel_upload(self, session: str) -> dict:
        return self.request("POST", f"/api/upload/{_path(session)}/cancel")

    def resolve_upload(self, session: str, checksum: str) -> dict:
        return self.request(
            "POST", f"/api/upload/{_path(session)}/resolve", {"checksum": checksum})

    # ---------------------------------------------------------------- 下载
    def prepare_download(self, path: str, client_public_key: str, preview: bool = False, client_ip: str = "") -> dict:
        """请求交付计划：只协商通道与密钥，不下载任何字节。

        client_public_key 是 base64 SPKI DER；密文计划会附带用该公钥封装的内容密钥。
        client_ip 可选，代传终端用户真实地址（仅 API Key 身份生效）：「后端申请、
        前端直取」时票据按它的网段绑定，123 回源鉴权才放行；不传则按后端 IP 绑定。
        """
        body = {"path": path, "clientPublicKey": client_public_key}
        if isinstance(client_ip, str) and client_ip.strip():
            body["clientIp"] = client_ip.strip()
        return self.request("POST", "/api/fs/preview" if preview else "/api/fs/download", body)

    def settle(self, ticket_id: str, completed: bool = True) -> dict:
        """回执交付票据；密文通道按预扣额度结算，completed 只表示是否完整接收。

        明文通道由服务端结算（带 Range 的中转流不结算），客户端回执会被拒绝。
        """
        if not ticket_id:
            return {}
        return self.request("POST", "/api/fs/settle", {
            "ticketId": ticket_id, "completed": completed,
        })

    @staticmethod
    def _ordered_parts(parts: list[dict], expected_size: int) -> list[dict]:
        if not isinstance(parts, list) or not parts:
            raise APIError(0, "分卷清单不正确", "")
        for part in parts:
            if not isinstance(part, dict):
                raise APIError(0, "分卷清单不正确", "")
            offset, size = part.get("offset"), part.get("size")
            if (not isinstance(part.get("url"), str) or not part["url"]
                    or not isinstance(offset, int) or isinstance(offset, bool)
                    or offset < 0 or not isinstance(size, int) or isinstance(size, bool)
                    or size <= 0):
                raise APIError(0, "分卷清单不正确", "")
        ordered = sorted(parts, key=lambda part: part["offset"])
        cursor = 0
        for part in ordered:
            if part["offset"] != cursor:
                raise APIError(0, "分卷清单存在缺口或重叠", "")
            cursor += part["size"]
        if expected_size > 0 and cursor != expected_size:
            raise APIError(0, "分卷清单无法覆盖完整密文", "")
        return ordered

    def _fetch_plain(self, plan: dict, dest: str) -> None:
        target = Path(dest)
        temp = tempfile.NamedTemporaryFile(
            prefix=f".{target.name}.", dir=str(target.parent), delete=False)
        temp_path = temp.name
        temp.close()
        headers = {"Authorization": f"Bearer {self.api_key}"}
        try:
            digest = hashlib.sha256()
            done = 0
            with _open_url(self._site_url(plan["streamUrl"]), headers) as src, \
                    open(temp_path, "wb") as out:
                while True:
                    chunk = src.read(1 << 20)
                    if not chunk:
                        break
                    digest.update(chunk)
                    done += len(chunk)
                    out.write(chunk)
            expected_size = int(plan.get("plainSize") or 0)
            if expected_size and done != expected_size:
                raise APIError(
                    0, f"内容长度不一致：期望 {expected_size} 字节，实得 {done} 字节", "")
            actual = digest.hexdigest()
            if plan.get("checksum") and actual != plan["checksum"]:
                raise APIError(
                    0, f"内容校验失败：期望 {plan['checksum']}，实得 {actual}", "")
            os.replace(temp_path, dest)
        except urllib.error.HTTPError as exc:
            raise _http_error(exc) from None
        except urllib.error.URLError as exc:
            raise _network_error(exc) from None
        finally:
            Path(temp_path).unlink(missing_ok=True)

    def download(self, path: str, dest: str, preview: bool = False) -> dict:
        """后端自己取字节并（密文计划）本地解密。票据按后端自身网段绑定，不接受
        代传访客 IP：若计划落到 mode=direct，取数方就是后端，绑到访客网段会让回源
        鉴权 403。需要「后端申请、前端直取」时改用 prepare_download 并代传 client_ip。
        """
        private_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        pub = private_key.public_key().public_bytes(
            serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
        plan = self.prepare_download(path, base64.b64encode(pub).decode("ascii"), preview)
        mode, meta = plan.get("mode"), plan.get("encryption") or {}
        if plan.get("contentForm") == "plaintext":
            if not plan.get("streamUrl"):
                raise APIError(0, "交付计划缺少可下载地址", "")
            self._fetch_plain(plan, dest)
            return plan
        if not meta.get("keyEnvelope"):
            raise APIError(0, "交付计划缺少解密材料", "")

        temp = tempfile.NamedTemporaryFile(prefix="xph-", delete=False)
        temp_path = temp.name
        temp.close()
        try:
            parts = plan.get("parts") or []
            expected_size = int(meta.get("cipherSize") or 0)
            if mode == "direct" and parts:
                self._download_parts(
                    self._ordered_parts(parts, expected_size), temp_path)
            elif mode == "direct" and plan.get("url"):
                self._fetch(plan["url"], temp_path)
            elif plan.get("streamUrl"):
                self._fetch(self._site_url(plan["streamUrl"]), temp_path, auth=True)
            else:
                raise APIError(0, "交付计划缺少可下载地址", "")
            self._decrypt_file(temp_path, dest, plan, private_key)
        finally:
            # 失败时不回执：密文票据一旦回执即按预扣全额结算，不回执则到期退款。
            Path(temp_path).unlink(missing_ok=True)

        ticket = plan.get("ticketId")
        if ticket:
            try:
                self.settle(ticket, True)
            except APIError:
                pass
        return plan

    def _decrypt_file(self, cipher_path: str, dest: str, plan: dict, private_key) -> None:
        meta = plan.get("encryption") or {}
        if not meta.get("keyEnvelope"):
            raise APIError(0, "交付计划缺少解密材料", "")
        with open(cipher_path, "rb") as src:
            header = src.read(64)
            if len(header) < 64 or header[:8] != b"XPHCRPT1":
                raise APIError(0, "密文文件头无效", "")
            if header[8] != 1:
                raise APIError(0, "密文文件头无效", f"不支持的加密算法标识 {header[8]}")
            block_log2 = header[9]
            if block_log2 < 9 or block_log2 > 26:
                raise APIError(0, "密文文件头无效", f"非法的块大小指数 {block_log2}")
            plain_size = int.from_bytes(header[28:36], "little")
            if plain_size > 1 << 62:
                raise APIError(0, "密文文件头无效", "非法的明文长度")
            nonce_prefix = int.from_bytes(header[36:40], "little")
            block = 1 << block_log2
            block_count = 0 if plain_size == 0 else (plain_size + block - 1) // block
            cipher_size = 64 + plain_size + block_count * 16

            problems = []
            if meta.get("chunkLog2") != block_log2:
                problems.append(f"块大小指数 头={block_log2} 记录={meta.get('chunkLog2')}")
            if meta.get("plainSize") != plain_size:
                problems.append(f"明文长度 头={plain_size} 记录={meta.get('plainSize')}")
            if meta.get("cipherSize") != cipher_size:
                problems.append(f"密文长度 头={cipher_size} 记录={meta.get('cipherSize')}")
            if meta.get("noncePrefix") != nonce_prefix:
                problems.append(f"nonce 前缀 头={nonce_prefix} 记录={meta.get('noncePrefix')}")
            if problems:
                raise APIError(0, "对象与记录不一致，已中止", "；".join(problems))
            actual_size = Path(cipher_path).stat().st_size
            if actual_size != cipher_size:
                raise APIError(
                    0, "密文长度与文件头不一致", f"头={cipher_size} 实际={actual_size}")

            dek = private_key.decrypt(
                base64.b64decode(meta["keyEnvelope"]),
                padding.OAEP(mgf=padding.MGF1(algorithm=hashes.SHA256()),
                             algorithm=hashes.SHA256(), label=None),
            )
            aes = AESGCM(dek)
            aad = header[:40]
            prefix = nonce_prefix.to_bytes(4, "big")
            target = Path(dest)
            temp = tempfile.NamedTemporaryFile(
                prefix=f".{target.name}.", dir=str(target.parent), delete=False)
            temp_path = temp.name
            temp.close()
            try:
                digest = hashlib.sha256()
                remaining = plain_size
                index = 0
                with open(temp_path, "wb") as out:
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
                expected_checksum = plan.get("checksum") or meta.get("checksum")
                if expected_checksum and digest.hexdigest() != expected_checksum:
                    raise APIError(
                        0, f"内容校验失败：期望 {expected_checksum}，实得与明文不一致", "")
                os.replace(temp_path, dest)
            except Exception:
                Path(temp_path).unlink(missing_ok=True)
                raise

    # ---------------------------------------------------------------- 分享
    def create_share(self, path: str, kind: str = "file", access_mode: str = "public",
                     password: str = "", expires_at: int = 0, allow_download: bool = True,
                     allow_preview: bool = True, allow_subpath: bool = True,
                     show_sharer_name: bool = True, custom_markdown: str = "",
                     max_deliveries: int = 0) -> dict:
        return self.request("POST", "/api/share", {
            "path": path, "kind": kind, "accessMode": access_mode,
            "password": password, "expiresAt": expires_at,
            "allowDownload": allow_download, "allowPreview": allow_preview,
            "allowSubpath": allow_subpath, "showSharerName": show_sharer_name,
            "customMarkdown": custom_markdown, "maxDeliveries": max_deliveries,
        })["share"]

    def list_shares(self, limit: int = 100, offset: int = 0) -> dict:
        query = urllib.parse.urlencode({"limit": limit, "offset": offset})
        return self.request("GET", f"/api/share?{query}")

    def update_share(self, share_id: str, *, custom_markdown: str | None = None,
                     path: str | None = None, kind: str | None = None,
                     access_mode: str | None = None, password: str | None = None,
                     allow_download: bool | None = None, allow_preview: bool | None = None,
                     allow_subpath: bool | None = None, show_sharer_name: bool | None = None,
                     expires_at: int | None = None, max_deliveries: int | None = None,
                     disabled: bool | None = None,
                     expect_updated_at: int | None = None) -> dict:
        """按字段局部更新；未传入的字段保持原值。"""
        payload = {
            "customMarkdown": custom_markdown,
            "path": path,
            "kind": kind,
            "accessMode": access_mode,
            "password": password,
            "allowDownload": allow_download,
            "allowPreview": allow_preview,
            "allowSubpath": allow_subpath,
            "showSharerName": show_sharer_name,
            "expiresAt": expires_at,
            "maxDeliveries": max_deliveries,
            "disabled": disabled,
            "expectUpdatedAt": expect_updated_at,
        }
        return self.request(
            "PATCH", f"/api/share/{_path(share_id)}",
            {key: value for key, value in payload.items() if value is not None},
        )["share"]

    def delete_share(self, share_id: str) -> dict:
        return self.request("DELETE", f"/api/share/{_path(share_id)}")

    def list_share_accesses(self, share_id: str, limit: int = 100, offset: int = 0,
                            snapshot_id: int | None = None) -> dict:
        query = {"limit": limit, "offset": offset}
        if snapshot_id is not None:
            query["snapshotId"] = snapshot_id
        return self.request(
            "GET",
            f"/api/share/{_path(share_id)}/accesses?{urllib.parse.urlencode(query)}",
        )

    def create_pickup(self, share_id: str, max_uses: int = 1) -> dict:
        return self.request(
            "POST", f"/api/share/{_path(share_id)}/pickup", {"maxUses": max_uses},
        )["pickup"]

    def list_pickups(self, share_id: str) -> dict:
        return self.request("GET", f"/api/share/{_path(share_id)}/pickup")

    def delete_pickup(self, code: str) -> dict:
        return self.request("DELETE", f"/api/pickup/{_path(code)}")

    def share_url(self, share_id: str) -> str:
        return f"{self.base_url}/s/{_path(share_id)}"

    # ---------------------------------------------------------------- 直链
    def create_direct_link(self, path: str, expires_at: int = 0, max_hits: int = 0) -> dict:
        return self.request("POST", "/api/direct-link", {
            "path": path, "expiresAt": expires_at, "maxHits": max_hits,
        })["link"]

    def list_direct_links(self, limit: int = 100, offset: int = 0) -> dict:
        query = urllib.parse.urlencode({"limit": limit, "offset": offset})
        return self.request("GET", f"/api/direct-link?{query}")

    def update_direct_link(self, link_id: str, *, expires_at: int | None = None,
                           max_hits: int | None = None, disabled: bool | None = None,
                           expect_updated_at: int | None = None) -> dict:
        payload = {
            "expiresAt": expires_at,
            "maxHits": max_hits,
            "disabled": disabled,
            "expectUpdatedAt": expect_updated_at,
        }
        return self.request(
            "PATCH", f"/api/direct-link/{_path(link_id)}",
            {key: value for key, value in payload.items() if value is not None},
        )["link"]

    def delete_direct_link(self, link_id: str) -> dict:
        return self.request("DELETE", f"/api/direct-link/{_path(link_id)}")

    def direct_link_target(self, link_id: str) -> str:
        return str(self.request("GET", f"/api/direct-link/{_path(link_id)}/target")["path"])

    def list_node_direct_links(self, checksums: list[str]) -> dict:
        query = urllib.parse.urlencode({"checksums": ",".join(checksums)})
        return self.request("GET", f"/api/direct-link/nodes?{query}")

    def direct_link_url(self, link_id: str) -> str:
        return f"{self.base_url}/d/{_path(link_id)}"

    # ---------------------------------------------------------------- 归档
    def list_archive(self, limit: int = 100, offset: int = 0) -> dict:
        query = urllib.parse.urlencode({"limit": limit, "offset": offset})
        return self.request("GET", f"/api/archive?{query}")

    def restore_archive(self, batch_id: str) -> dict:
        return self.request("POST", f"/api/archive/{_path(batch_id)}/restore")

    def clear_archive(self, batch_id: str) -> dict:
        return self.request("POST", f"/api/archive/{_path(batch_id)}/clear")

    def clear_all_archive(self) -> dict:
        return self.request("POST", "/api/archive/clear-all")

    # ---------------------------------------------------------------- 公告
    def list_announcements(self, kinds: list[str] | None = None) -> dict:
        query = urllib.parse.urlencode({"kinds": ",".join(kinds)}) if kinds else ""
        suffix = f"?{query}" if query else ""
        return self.request("GET", f"/api/announcements{suffix}")


if __name__ == "__main__":
    if len(sys.argv) == 2 and sys.argv[1] == "--version":
        print(sdk_version())
        raise SystemExit(0)
    if len(sys.argv) < 2:
        print("用法: python xiaoyuposthub_next_sdk_v5.py --version")
        print("      python xiaoyuposthub_next_sdk_v5.py <本地文件> [目标目录]")
        raise SystemExit(2)
    client = Client()
    local, parent = sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "/"
    result = client.upload(local, parent)
    print(json.dumps(result, ensure_ascii=False, indent=2))
