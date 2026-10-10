import assert from "node:assert/strict";
import test from "node:test";
import { Worker } from "node:worker_threads";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { createHash } from "node:crypto";
import { build } from "esbuild";

const root = resolve(new URL("..", import.meta.url).pathname);
const temporary = await mkdtemp(join(tmpdir(), "xph-transfer-"));
const bundle = join(temporary, "transfer.mjs");
await build({ entryPoints: [join(root, "src/delivery/transfer.worker.ts")], outfile: bundle,
  bundle: true, format: "esm", platform: "browser", alias: { "@": join(root, "src") }, logLevel: "silent" });
const bootstrap = join(temporary, "bootstrap.mjs");
await writeFile(bootstrap, `import { parentPort, workerData } from 'node:worker_threads';
  globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  globalThis.self = globalThis;
  globalThis.isSecureContext = !!workerData?.cipher;
  globalThis.fetch = async (_url, options) => {
    if (workerData?.cipher) {
      const [,start,end] = /bytes=(\\d+)-(\\d+)/.exec(options.headers.Range);
      return new Response(workerData.cipher.slice(Number(start), Number(end)+1), { status: 206 });
    }
    return new Response(new ReadableStream({
    start(controller) {
      for (let i = 0; i < 64; i++) controller.enqueue(new Uint8Array(65536).fill(91));
      controller.close();
    }
  })); };
  globalThis.postMessage = (value, transfers) => parentPort.postMessage(value, transfers);
  await import(${JSON.stringify(pathToFileURL(bundle).href)});
  parentPort.on('message', data => globalThis.onmessage({ data }));
  parentPort.postMessage({ type: 'ready' });`);
const contents = Buffer.alloc(4 * 1024 * 1024, 91);
const checksum = createHash("sha256").update(contents).digest("hex");
const streamUrl = "https://test.invalid/file";
test.after(async () => { await rm(temporary, { recursive: true }); });

async function delivery({ disk = false, corrupt = false, cancel = false } = {}) {
  const worker = new Worker(bootstrap);
  const phases = [];
  const written = [];
  let closed = false;
  let aborted = false;
  try {
    const outcome = await new Promise((resolve, reject) => {
      worker.on("error", reject);
      worker.on("message", message => {
        if (message.type === "ready") worker.postMessage({ type: "start", kind: "download", token: "", streamToDiskAbove: disk ? 0 : undefined });
        if (message.type === "progress") phases.push(message.value.phase);
        if (message.type === "request") {
          let value;
          if (message.method === "plan") {
            // 所有二进制工作都在实际 worker_threads 内执行。
            assert.equal(message.value, null);
            value = { fileName: "worker.bin", mimeType: "application/octet-stream", plainSize: contents.length,
              checksum: corrupt ? "bad" : checksum, mode: "proxy_decrypt", contentForm: "plaintext", streamUrl };
            if (cancel) worker.postMessage({ type: "cancel" });
          }
          if (message.method === "open") value = "worker.bin";
          if (message.method === "write") written.push(message.value);
          if (message.method === "close") closed = true;
          if (message.method === "abort") aborted = true;
          worker.postMessage({ type: "reply", id: message.id, value });
        }
        if (message.type === "result" || message.type === "error") resolve(message);
      });
    });
    return { outcome, phases, written, closed, aborted };
  } finally { await worker.terminate(); }
}
test("实际独立线程完成下载、SHA256 校验及 Blob 交付", async () => {
  const { outcome, phases } = await delivery({});
  assert.equal(outcome.type, "result");
  assert.equal(outcome.value.checksum, checksum);
  assert.deepEqual(Buffer.from(await outcome.value.blob.arrayBuffer()), contents);
  assert.deepEqual([...new Set(phases)], ["preparing", "fetching", "verifying", "delivering", "done"]);
});
test("流式落盘消息按顺序写入，校验后才关闭", async () => {
  const { outcome, written, closed, aborted } = await delivery({ disk: true });
  assert.equal(outcome.type, "result");
  assert.equal(outcome.value.blob, null);
  assert.deepEqual(Buffer.concat(written), contents);
  assert.equal(closed, true);
  assert.equal(aborted, false);
});
test("校验不通过会中止保存通道，不交付错误文件", async () => {
  const { outcome, closed, aborted } = await delivery({ disk: true, corrupt: true });
  assert.equal(outcome.type, "error");
  assert.match(outcome.error.message, /内容校验失败/);
  assert.equal(closed, false);
  assert.equal(aborted, true);
});
test("准备阶段取消会在交付计划返回后中止，不取文件", async () => {
  const { outcome, phases } = await delivery({ cancel: true });
  assert.equal(outcome.type, "error");
  assert.equal(outcome.error.name, "AbortError");
  assert.deepEqual(phases, ["preparing"]);
});

const uploadBundle = join(temporary, "upload.mjs");
await build({ entryPoints: [join(root, "src/delivery/transfer.worker.ts")], outfile: uploadBundle,
  bundle: true, format: "esm", platform: "browser", alias: { "@": join(root, "src") }, logLevel: "silent",
  plugins: [{ name: "upload-server", setup(build) {
    build.onResolve({ filter: /^@\/api\/endpoints$/ }, () => ({ path: "endpoints", namespace: "mock" }));
    build.onLoad({ filter: /.*/, namespace: "mock" }, () => ({ contents: `
      import { createHash } from 'node:crypto';
      const chunks = new Map();
      export const fsApi = {}, fetchCipherRange = () => {}, streamUrlWithToken = x => x;
      export const uploadApi = {
        async init(input) {
          if (input.name !== 'upload.bin') throw new Error('文件名未保留');
          return { sessionId: 'thread-session', chunkSize: 1048576, chunkTotal: 4 };
        },
        async chunk(_id, index, blob, signal, report) {
          signal.throwIfAborted();
          chunks.set(index, Buffer.from(await blob.arrayBuffer()));
          report(blob.size, blob.size);
        },
        async resolve(_id, checksum) {
          if (checksum !== ${JSON.stringify(checksum)}) throw new Error('摘要错误');
          return { dedup: false };
        },
        async complete() {
          const bytes = Buffer.concat([...chunks].sort((a,b) => a[0]-b[0]).map(x => x[1]));
          if (createHash('sha256').update(bytes).digest('hex') !== ${JSON.stringify(checksum)}) throw new Error('分片错误');
          return { state: 'queued', totalBytes: bytes.length };
        },
        async status() { return { state: "done", node: { id: "uploaded-node" } }; },
        async cancel() { return true; }
      };`, loader: "js" }));
  }}], external: ["node:crypto"] });
const uploadBootstrap = join(temporary, "upload-bootstrap.mjs");
await writeFile(uploadBootstrap, `import { parentPort } from 'node:worker_threads';
  import { File } from 'node:buffer';
  globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
  globalThis.self = globalThis;
  globalThis.postMessage = value => parentPort.postMessage(value);
  await import(${JSON.stringify(pathToFileURL(uploadBundle).href)});
  parentPort.on('message', data => {
    if (data.type === 'start') data.file = new File([data.file], 'upload.bin', { lastModified: 1 });
    globalThis.onmessage({ data });
  });
  parentPort.postMessage({ type: 'ready' });`);
test("上传线程执行完整摘要、分片与收尾，并同步续传记录", async () => {
  const worker = new Worker(uploadBootstrap);
  const storage = [];
  const phases = [];
  const progress = [];
  try {
    const result = await new Promise((resolve, reject) => {
      worker.on("error", reject);
      worker.on("message", message => {
        if (message.type === "ready") worker.postMessage({ type: "start", kind: "upload", token: "", file: new Blob([contents]), parentPath: "/", sessions: [] });
        if (message.type === "storage") storage.push(message);
        if (message.type === "progress") { phases.push(message.value.phase); progress.push(message.value); }
        if (message.type === "error") reject(new Error(message.error.message));
        if (message.type === "result") resolve(message.value);
      });
    });
    assert.equal(result.node.id, "uploaded-node");
    assert.equal(result.dedup, false);
    assert.ok(phases.includes("hashing"));
    assert.ok(phases.includes("uploading"));
    assert.ok(phases.includes("finishing"));
    assert.ok(progress.some(info => info.phase === "finishing" && info.canCancel === true), "收尾队列取消权限必须立即同步");
    assert.equal(phases.at(-1), "done");
    assert.ok(storage.some(message => message.value && JSON.parse(message.value).checksum === checksum));
    assert.equal(storage.at(-1).value, null);
  } finally { await worker.terminate(); }
});

test("安全上下文线程保留 RSA 私钥并完成 AES 解密、校验和交付", async () => {
  const { parseXphHeader } = await import("../src/crypto/xph.ts");
  const plain = new Uint8Array(2048).fill(73);
  const raw = new Uint8Array(64);
  raw.set(new TextEncoder().encode("XPHCRPT1")); raw[8] = 1; raw[9] = 9;
  new DataView(raw.buffer).setBigUint64(28, BigInt(plain.length), true);
  const header = parseXphHeader(raw);
  const dek = crypto.getRandomValues(new Uint8Array(32));
  const key = await crypto.subtle.importKey("raw", dek, "AES-GCM", false, ["encrypt"]);
  const cipher = new Uint8Array(header.cipherSize); cipher.set(raw);
  for (let i = 0; i < 4; i++) {
    const iv = new Uint8Array(12); new DataView(iv.buffer).setBigUint64(4, BigInt(i), false);
    cipher.set(new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv, additionalData: header.aad }, key, plain.slice(i*512,(i+1)*512))), 64+i*528);
  }
  const worker = new Worker(bootstrap, { workerData: { cipher } });
  try {
    const outcome = await new Promise((resolve, reject) => {
      worker.on("error", reject);
      worker.on("message", async message => {
        try {
          if (message.type === "ready") worker.postMessage({ type: "start", kind: "download", token: "" });
          if (message.type === "request" && message.method === "plan") {
            assert.equal(typeof message.value, "string");
            const publicKey = await crypto.subtle.importKey("spki", Buffer.from(message.value, "base64"), { name: "RSA-OAEP", hash: "SHA-256" }, false, ["encrypt"]);
            const keyEnvelope = Buffer.from(await crypto.subtle.encrypt("RSA-OAEP", publicKey, dek)).toString("base64");
            worker.postMessage({ type: "reply", id: message.id, value: {
              fileName: "encrypted.bin", plainSize: plain.length, mimeType: "application/octet-stream",
              mode: "direct", contentForm: "ciphertext", url: streamUrl,
              checksum: createHash("sha256").update(plain).digest("hex"),
              encryption: { chunkLog2: 9, plainSize: plain.length, cipherSize: cipher.length, noncePrefix: 0, keyEnvelope },
            } });
          }
          if (message.type === "error") reject(new Error(message.error.message));
          if (message.type === "result") resolve(message.value);
        } catch (error) { reject(error); }
      });
    });
    assert.deepEqual(new Uint8Array(await outcome.blob.arrayBuffer()), plain);
  } finally { await worker.terminate(); }
});
