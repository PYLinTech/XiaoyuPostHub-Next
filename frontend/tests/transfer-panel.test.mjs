import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { compileScript, parse } from "@vue/compiler-sfc";
import { createRenderer, nextTick } from "vue";
import ts from "typescript";

const moduleUrl = code => `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
const vueUrl = import.meta.resolve("vue");
const progressUrl = moduleUrl(ts.transpileModule(await readFile(new URL("../src/lib/transferProgress.ts", import.meta.url), "utf8"), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 },
}).outputText);
const stateUrl = moduleUrl(`import { reactive, ref } from ${JSON.stringify(vueUrl)};
export const transferPanel=reactive({visible:false,collapsed:false,selected:'upload'});
export const uploadItems=ref([]),downloadItems=reactive([]);
export const serverUploadItems=reactive([]);
export const useUploads=()=>({items:uploadItems,activeCount:ref(0),cancelUploadItem(){},retryUpload(){},removeUpload(){}});
export const useDownloads=()=>({items:downloadItems,remove(){}});
export const useServerUploadTasks=()=>({items:serverUploadItems,cancel(){},remove(){}});
export const canCancelDownload=()=>false;
export const formatBytes=n=>String(n);
export { isByteTransfer, aggregateTransferProgress } from ${JSON.stringify(progressUrl)};
export default {render(){return null}};`);
const state = await import(stateUrl);
const descriptor = parse(await readFile(new URL("../src/components/AppTransferDock.vue", import.meta.url), "utf8")).descriptor;
let script = compileScript(descriptor, { id: "panel-regression", inlineTemplate: true }).content;
script = ts.transpileModule(script, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
script = script.replace(/from ["']([^"']+)["']/g, (_, name) => `from ${JSON.stringify(name === "vue" ? vueUrl : stateUrl)}`);
const component = (await import(moduleUrl(script))).default;
const node = text => ({ text, children: [], parent: null, style: {}, clientHeight: 300, scrollHeight: 300, scrollTop: 0 });
function remove(el) { if(el.parent) el.parent.children.splice(el.parent.children.indexOf(el), 1); el.parent = null; }
const renderer = createRenderer({
  createElement: () => node(""), createText: node, createComment: () => node(""),
  setText: (el, text) => { el.text = text; }, setElementText: (el, text) => { el.text = text; el.children = []; },
  parentNode: el => el.parent, nextSibling: el => el.parent?.children[el.parent.children.indexOf(el) + 1],
  patchProp: () => {},
  insert(el, parent, anchor) { if(el.parent) remove(el); el.parent = parent; const i = parent.children.indexOf(anchor); parent.children.splice(i < 0 ? parent.children.length : i, 0, el); },
  remove,
});
const text = el => el.text + el.children.map(text).join("");

test("真实面板保留空标签与收起状态，最后一个任务移除后隐藏", async () => {
  const previous = globalThis.ResizeObserver;
  globalThis.ResizeObserver = class { observe() {} disconnect() {} };
  const root = node(""), app = renderer.createApp(component);
  try {
    app.mount(root);
    assert.equal(text(root), "");
    state.uploadItems.value.push({id:'1',file:{name:'文件.zip',size:100},status:'running',progress:{phase:'uploading',sent:30,ratio:.3},errorMessage:''});
    state.transferPanel.visible = true;
    await nextTick();
    assert.match(text(root), /文件.zip/);
    state.transferPanel.selected = 'download';
    await nextTick();
    assert.match(text(root), /暂无下载任务/);
    assert.equal(state.transferPanel.visible, true);
    state.transferPanel.collapsed = true;
    await nextTick();
    assert.match(text(root), /正在传输/);
    state.uploadItems.value[0].progress.phase = 'finishing';
    await nextTick();
    assert.doesNotMatch(text(root), /正在传输/);
    assert.match(text(root), /传输任务/);
    assert.equal(state.transferPanel.collapsed, true);
    state.uploadItems.value.splice(0);
    await nextTick();
    assert.equal(state.transferPanel.visible, false);
    assert.equal(text(root), "");
  } finally { app.unmount(); globalThis.ResizeObserver = previous; }
});
