import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import ts from "typescript";
const encode = source => `data:text/javascript;base64,${Buffer.from(source).toString("base64")}`;
async function load(path, replacements = {}) {
  const source = await readFile(new URL(path, import.meta.url), "utf8");
  let js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  for (const [name, url] of Object.entries(replacements)) js = js.replace(`from "${name}"`, `from ${JSON.stringify(url)}`);
  return encode(js);
}
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
const api = await import(await load("../src/api/client.ts"));
test("线程、JSON、Blob 和上传 401 走同一个去重出口，重新登录后可再次通知", async () => {
  let count = 0;
  api.setUnauthorizedHandler(() => { count++; });
  globalThis.fetch = async () => new Response(JSON.stringify({ error: { message: "未授权" } }), { status: 401 });
  api.setToken("first");
  api.notifyUnauthorized();
  await assert.rejects(api.request("/test"), error => error.status === 401);
  await assert.rejects(api.requestBlob("/test", {}), error => error.status === 401);
  globalThis.XMLHttpRequest = class {
    upload = {};
    status = 401;
    responseText = '{}';
    open() {} setRequestHeader() {} getResponseHeader() { return null; }
    send() { this.onload(); }
  };
  await assert.rejects(api.requestWithUploadProgress("/test", new Blob()), error => error.status === 401);
  assert.equal(count, 1);
  api.setToken("second");
  await assert.rejects(api.request("/test", { clearSessionOn401: false }), error => error.status === 401);
  assert.equal(count, 1);
  await assert.rejects(api.request("/test"), error => error.status === 401);
  assert.equal(count, 2);
});
const settlement = encode('export const settled=[]; export const settleQuietly=async plan=>{settled.push(plan);}; export const assertHeaderMatchesMeta=()=>{}; export const cipherSourceUrl=()=>""; export const fetchCipherPlanRange=()=>{throw new Error("取消后仍然取数");};');
const { preparePreview } = await import(await load("../src/delivery/preview.ts", {
  "@/api/endpoints": encode('export const streamUrlWithToken=x=>x;'),
  "@/crypto/clientkey": encode('export const encryptionSupported=()=>false; export const createClientKeyPair=()=>null; export const resolveContentKey=()=>null;'),
  "@/crypto/xph": encode('export const bytesToBase64=()=>""; export const decryptAll=()=>{}; export const importContentKey=()=>{}; export const parseXphHeader=()=>{}; export const HEADER_SIZE=64;'),
  "./download": settlement,
}));
test("关闭预览后不会申请新计划；申请期间取消会结算票据而不继续建预览", async () => {
  const before = new AbortController(); before.abort();
  await assert.rejects(preparePreview({ plan: () => { throw new Error("不应调用"); } }, { signal: before.signal }), { name: "AbortError" });
  const during = new AbortController();
  await assert.rejects(preparePreview({ plan: async () => { during.abort(); return { ticketId: "preview-ticket" }; } }, { signal: during.signal }), { name: "AbortError" });
  assert.equal((await import(settlement)).settled.at(-1).ticketId, "preview-ticket");
});
const notifications = encode('export const errors=[]; export const useToasts=()=>({error:(...args)=>errors.push(args)});');
const asyncLib = encode('export const logs=[]; export const isAbortError=e=>e.name==="AbortError"; export const describeError=e=>e.message; export const logError=(context,error)=>logs.push({context,error});');
const uploads = await import(await load("../src/stores/uploads.ts", {
  "vue": import.meta.resolve("vue"),
  "@/delivery/transferClient": encode('export const uploadFile=async()=>({node:{id:"node"},dedup:false});'),
  "@/delivery/upload": encode('export const cancelUpload=async()=>true;'),
  "@/lib/async": asyncLib,
  "./session": encode('export const useSession=()=>({state:{upload:{maxTasks:1,maxConcurrency:1}}});'),
  "./toast": notifications,
  "./transferPanel": encode('export const showTransfer=()=>{};'),
}));
test("上传完成的刷新回调异常不改变成功状态，运行任务不能直接移除", async () => {
  const offFirst = uploads.onUploadComplete(() => { throw new Error("同步刷新失败"); });
  const offSecond = uploads.onUploadComplete(async () => { throw new Error("异步刷新失败"); });
  let notified = false;
  const offThird = uploads.onUploadComplete(() => { notified = true; });
  const file = new Blob(["file"]); file.name = "file";
  const [id] = uploads.enqueueUploads([file], "/");
  uploads.removeUpload(id);
  assert.equal(uploads.useUploads().items.value.length, 1);
  await new Promise(resolve => setImmediate(resolve));
  const item = uploads.useUploads().items.value[0];
  assert.equal(item.status, "done");
  assert.equal(item.node.id, "node");
  assert.equal(notified, true);
  assert.equal((await import(asyncLib)).logs.length, 2);
  assert.equal((await import(notifications)).errors.length, 0);
  uploads.removeUpload(id);
  assert.equal(uploads.useUploads().items.value.length, 0);
  offFirst(); offSecond(); offThird();
});
const cancelBackend = encode('export const waiting=[]; export const cancelUpload=()=>new Promise(resolve=>waiting.push(resolve));');
const cancelStore = await import(await load("../src/stores/uploads.ts", {
  "vue": import.meta.resolve("vue"),
  "@/delivery/transferClient": encode('export const uploadFile=()=>new Promise(()=>{});'),
  "@/delivery/upload": cancelBackend, "@/lib/async": asyncLib,
  "./session": encode('export const useSession=()=>({state:{upload:{maxTasks:1,maxConcurrency:1}}});'),
  "./toast": notifications, "./transferPanel": encode('export const showTransfer=()=>{};'),
}));
test("迟到的取消响应不会中止同一条目的新执行，已结束条目不发取消请求", async () => {
  const file = new Blob(["file"]); file.name = "cancel-file";
  const [id] = cancelStore.enqueueUploads([file], "/");
  const item = cancelStore.useUploads().items.value[0];
  item.progress.phase = "finishing"; item.sessionId = "old-session";
  const canceling = cancelStore.cancelUploadItem(id);
  const newer = new AbortController();
  item.controller = newer; item.sessionId = "new-session";
  const backend = await import(cancelBackend);
  backend.waiting[0](true);
  await canceling;
  assert.equal(newer.signal.aborted, false);
  item.status = "done";
  await cancelStore.cancelUploadItem(id);
  assert.equal(backend.waiting.length, 1);
  cancelStore.removeUpload(id);
});
const uploadEndpoints = encode(`import { ApiError } from ${JSON.stringify(await load("../src/api/client.ts"))};
export let initialized=0; export let attempts=0;
export const uploadApi={
 async init() { initialized++; return {sessionId:'retry-session',chunkSize:100,chunkTotal:1}; },
 async chunk(_id,_index,_blob,_signal,report) { attempts++; if(attempts===1) {report(80,100); throw new ApiError(503,'暂时失败');} report(10,100); report(100,100); },
 async resolve() {return {dedup:false};}, async complete() {return {state:'done',node:{id:'retry-node'}};}, async cancel() {},
};`);
const uploadCore = await import(await load("../src/delivery/upload.ts", {
  "@/api/client": await load("../src/api/client.ts"),
  "@/api/endpoints": uploadEndpoints,
  "@/crypto/sha256": await load("../src/crypto/sha256.ts"),
  "@/lib/async": asyncLib,
}));
test("上传提前取消不建立会话；失败分片重传从零计算本次发送进度", async () => {
  const file = new Blob([new Uint8Array(100)]); file.name='retry.bin'; file.lastModified=1;
  const canceled = new AbortController(); canceled.abort();
  await assert.rejects(uploadCore.uploadFile(file, {parentPath:'/',signal:canceled.signal}), {name:'AbortError'});
  assert.equal((await import(uploadEndpoints)).initialized,0);
  const sent=[];
  const timer=globalThis.setTimeout;
  globalThis.setTimeout=callback=>{queueMicrotask(callback);return 0;};
  try {
    const result=await uploadCore.uploadFile(file,{parentPath:'/',onProgress:info=>{if(info.phase==='uploading')sent.push(info.sent);}});
    assert.equal(result.node.id,'retry-node');
    const failed=sent.lastIndexOf(80);
    assert.ok(failed>=0);
    assert.equal(sent[failed+1],0);
    assert.ok(sent.slice(failed+1).includes(10));
    assert.equal((await import(uploadEndpoints)).attempts,2);
  } finally {globalThis.setTimeout=timer;}
});
