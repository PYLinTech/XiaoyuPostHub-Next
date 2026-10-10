import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import ts from "typescript";
const encode = source => `data:text/javascript;base64,${Buffer.from(source).toString("base64")}`;
async function load(file, replacements = {}) {
  const source = await readFile(new URL(`../src/delivery/${file}.ts`, import.meta.url), "utf8");
  let compiled = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  compiled = compiled.replace('new URL("./transfer.worker.ts", import.meta.url)', 'new URL("file:///transfer.worker.js")');
  for (const [name, url] of Object.entries(replacements)) compiled = compiled.replace(`from "${name}"`, `from ${JSON.stringify(url)}`);
  return encode(compiled);
}
const api = encode(`export class ApiError extends Error {
  constructor(status, message, detail, retryAfter) { super(message); this.name='ApiError'; Object.assign(this,{status,detail,retryAfter}); }
} export let token='token'; export let notices=0; export const setToken=value=>{token=value;}; export const getToken=()=>token; export const notifyUnauthorized=()=>{notices++;};`);
const protocol = await load("transferProtocol", { "@/api/client": api });
const scheduler = await load("transferScheduler");
const saveModule = encode("export const settled=[]; export const tryOpenDiskSink=async()=>null; export const settleQuietly=async plan=>{settled.push(plan);};");
const client = await load("transferClient", {
  "@/api/client": api, "./transferProtocol": protocol, "./transferScheduler": scheduler,
  "./download": saveModule,
});
const { uploadFile, runDelivery } = await import(client);
class FakeWorker {
  static instances = [];
  sent = [];
  terminated = false;
  constructor() { FakeWorker.instances.push(this); }
  postMessage(message) { this.sent.push(message); }
  terminate() { this.terminated = true; }
  emit(message) { return this.onmessage({ data: message }); }
}
globalThis.Worker = FakeWorker;
const records = new Map();
globalThis.localStorage = {
  get length() { return records.size; }, key: i => [...records.keys()][i] ?? null,
  getItem: key => records.get(key) ?? null, setItem: (key,value) => records.set(key,value), removeItem: key => records.delete(key),
};
const tick = () => new Promise(resolve => setImmediate(resolve));

test("显示层只接收上传快照、会话记录与结果，完成后释放线程", async () => {
  const progress = [];
  const sessions = [];
  records.set("xph.upload.existing", "record");
  const promise = uploadFile(new Blob(["file"]), { parentPath: "/", onProgress: p => progress.push(p), onSessionId: id => sessions.push(id) });
  await tick();
  const worker = FakeWorker.instances.at(-1);
  assert.equal(worker.sent[0].token, "token");
  assert.deepEqual(worker.sent[0].sessions, [["xph.upload.existing", "record"]]);
  await worker.emit({ type: "session", value: "session" });
  await worker.emit({ type: "storage", key: "xph.upload.session", value: "record" });
  await worker.emit({ type: "progress", value: { phase: "uploading", sent: 1 } });
  await worker.emit({ type: "result", value: { dedup: false, node: { id: 1 } } });
  assert.equal((await promise).node.id, 1);
  assert.deepEqual(sessions, ["session"]);
  assert.equal(progress[0].sent, 1);
  assert.equal(records.get("xph.upload.session"), "record");
  assert.equal(worker.terminated, true);
});
test("取消指令交给执行线程处理，保持票据清理时间，恢复 AbortError", async () => {
  const controller = new AbortController();
  const promise = runDelivery({ plan: async () => ({}) }, { signal: controller.signal });
  const rejection = assert.rejects(promise, { name: "AbortError" });
  await tick();
  const worker = FakeWorker.instances.at(-1);
  controller.abort();
  assert.equal(worker.sent.at(-1).type, "cancel");
  assert.equal(worker.terminated, false);
  await worker.emit({ type: "error", error: { name: "AbortError", message: "已取消" } });
  await rejection;
  assert.equal(worker.terminated, true);
});
test("计划调用仅交换公钥，API 错误保留状态码和重试信息", async () => {
  let publicKey;
  const promise = runDelivery({ plan: async pair => { publicKey = pair.publicKeyBase64; return { fileName: "file" }; } });
  const rejection = assert.rejects(promise, error => error.name === "ApiError" && error.status === 429 && error.retryAfter === 2);
  await tick();
  const worker = FakeWorker.instances.at(-1);
  await worker.emit({ type: "request", method: "plan", value: "public-key", id: 1 });
  assert.equal(publicKey, "public-key");
  assert.equal(worker.sent.at(-1).value.fileName, "file");
  await worker.emit({ type: "error", error: { name: "ApiError", message: "限流", status: 429, retryAfter: 2 } });
  await rejection;
});
test("计划返回前线程异常退出，迟到的票据不会留在未结算状态", async () => {
  let finishPlan;
  const promise = runDelivery({ plan: () => new Promise(resolve => { finishPlan = resolve; }) });
  const rejection = assert.rejects(promise, /线程崩溃/);
  await tick();
  const worker = FakeWorker.instances.at(-1);
  const response = worker.emit({ type: "request", method: "plan", value: null, id: 8 });
  worker.onerror({ message: "线程崩溃" });
  await rejection;
  finishPlan({ ticketId: "late-ticket" });
  await response;
  const { settled } = await import(saveModule);
  assert.equal(settled.at(-1).ticketId, "late-ticket");
  assert.equal(worker.sent.filter(message => message.type === "reply").length, 0);
});
test("上传等待服务器收尾时让出处理额度，不阻塞下载", async () => {
  const first = uploadFile(new Blob(["a"]), { parentPath: "/" });
  const second = uploadFile(new Blob(["b"]), { parentPath: "/" });
  await tick();
  const [firstWorker, secondWorker] = FakeWorker.instances.slice(-2);
  const before = FakeWorker.instances.length;
  const third = runDelivery({ plan: async () => ({}) });
  await tick();
  assert.equal(FakeWorker.instances.length, before);
  await firstWorker.emit({ type: "progress", value: { phase: "finishing" } });
  await tick();
  assert.equal(FakeWorker.instances.length, before + 1);
  const thirdWorker = FakeWorker.instances.at(-1);
  for (const worker of [firstWorker, secondWorker, thirdWorker]) await worker.emit({ type: "result", value: {} });
  await Promise.all([first, second, third]);
});
test("显示回调异常先取消执行，清理后返回原始错误而不挂起", async () => {
  const failure = new Error("显示回调失败");
  const controller = new AbortController();
  const promise = runDelivery({ plan: async () => ({}) }, { signal: controller.signal, onProgress() { throw failure; } });
  const rejection = assert.rejects(promise, error => error === failure);
  await tick();
  const worker = FakeWorker.instances.at(-1);
  await worker.emit({ type: "progress", value: { phase: "fetching" } });
  assert.equal(worker.sent.at(-1).type, "cancel");
  assert.equal(worker.terminated, false);
  await worker.emit({ type: "error", error: { name: "AbortError", message: "已取消" } });
  await rejection;
  const count = worker.sent.length;
  controller.abort();
  assert.equal(worker.sent.length, count);
});
test("取消消息发送失败仍终止线程并释放调度额度", async () => {
  const controller = new AbortController();
  const promise = runDelivery({ plan: async () => ({}) }, { signal: controller.signal });
  const rejection = assert.rejects(promise, /发送失败/);
  await tick();
  const worker = FakeWorker.instances.at(-1);
  worker.postMessage = () => { throw new Error("发送失败"); };
  controller.abort();
  await rejection;
  assert.equal(worker.terminated, true);
});
test("保存选择迟于线程退出时返回，仍会中止保存通道", async () => {
  let finishOpen;
  let aborted = 0;
  const promise = runDelivery({ plan: async () => ({}) }, { openDiskSink: () => new Promise(resolve => { finishOpen = resolve; }) });
  const rejection = assert.rejects(promise, /保存期间异常/);
  await tick();
  const worker = FakeWorker.instances.at(-1);
  const opening = worker.emit({ type: "request", method: "open", value: "file", id: 9 });
  worker.onerror({ message: "保存期间异常" });
  await rejection;
  finishOpen({ name: "file", abort: async () => { aborted++; } });
  await opening;
  assert.equal(aborted, 1);
  assert.equal(worker.sent.filter(message => message.type === "reply").length, 0);
});
test("旧线程的 401 不清除后来登录的会话", async () => {
  const session = await import(api);
  session.setToken("old-token");
  const promise = runDelivery({ plan: async () => ({}) });
  await tick();
  const worker = FakeWorker.instances.at(-1);
  session.setToken("new-token");
  await worker.emit({ type: "unauthorized", token: "old-token" });
  assert.equal(session.notices, 0);
  await worker.emit({ type: "unauthorized", token: "new-token" });
  assert.equal(session.notices, 1);
  await worker.emit({ type: "result", value: {} });
  await promise;
});
