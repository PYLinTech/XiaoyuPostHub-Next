import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import ts from "typescript";

async function loadSource(path, replacements = {}) {
  const source = await readFile(new URL(path, import.meta.url), "utf8");
  let compiled = ts.transpileModule(source, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 },
  }).outputText.replace('from "vue"', `from ${JSON.stringify(import.meta.resolve("vue"))}`);
  for (const [specifier, url] of Object.entries(replacements)) {
    compiled = compiled.replace(`from "${specifier}"`, `from ${JSON.stringify(url)}`);
  }
  return `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`;
}
const panelUrl = await loadSource("../src/stores/transferPanel.ts");
const { transferPanel, showTransfer } = await import(panelUrl);
const downloadsUrl = await loadSource("../src/stores/downloads.ts", { "./transferPanel": panelUrl });
const { addDownload, useDownloads, canCancelDownload } = await import(downloadsUrl);

test("首个任务展开，后续任务保留用户收起状态", () => {
  transferPanel.visible = false;
  showTransfer("upload");
  transferPanel.collapsed = true;
  const item = addDownload("文件.pdf", () => {});
  assert.equal(transferPanel.selected, "download");
  assert.equal(transferPanel.collapsed, true);
  transferPanel.selected = "upload";
  item.progress.bytesDone = 100;
  assert.equal(transferPanel.selected, "upload");
  transferPanel.collapsed = true;
  showTransfer("upload");
  assert.equal(transferPanel.collapsed, true);
  transferPanel.visible = false;
  showTransfer("download");
  assert.equal(transferPanel.collapsed, false);
  item.status = "done";
  useDownloads().clearFinished();
});

test("清除已结束任务保留运行中的下载与取消操作", () => {
  let canceled = false;
  const active = addDownload("下载中的文件.pdf", () => { canceled = true; });
  const finished = addDownload("完成的文件.pdf", () => {});
  finished.status = "done";
  const failed = addDownload("失败的文件.pdf", () => {});
  failed.status = "error";
  const downloads = useDownloads();
  downloads.remove(active.id);
  assert.equal(downloads.items.length, 3);
  downloads.clearFinished();
  assert.deepEqual(downloads.items.map(item => item.id), [active.id]);
  assert.equal(canceled, false);
  downloads.items[0].cancel();
  assert.equal(canceled, true);
  active.status = "canceled";
  downloads.remove(active.id);
  assert.equal(downloads.items.length, 0);
});

test("开始保存和完成后的任务不再允许取消", () => {
  const item = addDownload("文件.pdf", () => {});
  assert.equal(canCancelDownload(item), true);
  item.progress.phase = "delivering";
  assert.equal(canCancelDownload(item), false);
  item.progress.phase = "done";
  assert.equal(canCancelDownload(item), false);
  item.status = "done";
  useDownloads().clearFinished();
});

const moduleUrl = code => `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
const apiUrl = moduleUrl(`export const settled = []; export const fsApi = { settle: async id => settled.push(id) }; export const streamUrlWithToken = url => url; export const fetchCipherRange = () => { throw new Error("取消后仍然取数"); };`);
const deliveryUrl = await loadSource("../src/delivery/download.ts", {
  "@/api/endpoints": apiUrl,
  "@/crypto/clientkey": moduleUrl("export const encryptionSupported = () => false; export const createClientKeyPair = () => null; export const resolveContentKey = () => null;"),
  "@/crypto/xph": moduleUrl("export const decryptAll = () => {}; export const importContentKey = () => {}; export const parseXphHeader = () => {}; export const HEADER_SIZE = 64;"),
  "@/crypto/sha256": moduleUrl("export class Sha256 {} export const bytesToHex = () => '';"),
});
const { runDelivery } = await import(deliveryUrl);
const { settled } = await import(apiUrl);

test("下载开始前取消不会申请交付计划", async () => {
  const controller = new AbortController();
  controller.abort();
  let planned = false;
  await assert.rejects(runDelivery({ plan: async () => { planned = true; } }, { signal: controller.signal }), { name: "AbortError" });
  assert.equal(planned, false);
});

test("准备期间取消会结算已申请票据并停止取数", async () => {
  const controller = new AbortController();
  await assert.rejects(runDelivery({ plan: async () => {
    controller.abort();
    return { ticketId: "canceled-ticket", mode: "proxy", contentForm: "ciphertext" };
  } }, { signal: controller.signal }), { name: "AbortError" });
  assert.deepEqual(settled, ["canceled-ticket"]);
});

const aggregateUrl = await loadSource("../src/lib/transferProgress.ts");
const { aggregateTransferProgress, isByteTransfer } = await import(aggregateUrl);
test("传输总进度按字节加权，空任务无进度且完成字节只显示99%", () => {
  assert.equal(aggregateTransferProgress([]), null);
  assert.equal(aggregateTransferProgress([{done: 0,total: 0}]), null);
  assert.equal(aggregateTransferProgress([{done: 90,total: 100},{done: 0,total: 900}]), .09);
  assert.equal(aggregateTransferProgress([{done: 100,total: 100}]), .99);
  assert.equal(aggregateTransferProgress([{done: -10,total: 100}]), 0);
});

test("仅正在传输的阶段参与总进度，准备、后处理和异常被剔除", () => {
  for (const phase of ["uploading", "fetching", "decrypting"]) assert.equal(isByteTransfer("running", phase), true);
  for (const phase of ["hashing", "preparing", "finishing", "verifying", "delivering", "done"]) assert.equal(isByteTransfer("running", phase), false);
  for (const status of ["queued", "error", "canceled", "done"]) assert.equal(isByteTransfer(status, "uploading"), false);
});
