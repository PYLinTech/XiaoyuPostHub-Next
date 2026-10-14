import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import ts from 'typescript';
import { JSDOM } from 'jsdom';
import { compileScript, parse } from '@vue/compiler-sfc';

const dom = new JSDOM('<div id="app"></div>', { url: 'https://example.test/files' });
for (const key of ['window', 'document', 'HTMLElement', 'SVGElement', 'Element', 'Node']) globalThis[key] = key === 'window' ? dom.window : dom.window[key];
Object.defineProperty(globalThis, 'navigator', { configurable: true, value: dom.window.navigator });
globalThis.requestAnimationFrame = callback => setTimeout(callback, 0);
globalThis.cancelAnimationFrame = clearTimeout;
const { createApp, nextTick, reactive } = await import('vue');
const vue = import.meta.resolve('vue');
const url = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`;
const transpile = code => ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
const calls = { tasks: 0, plans: 0, saves: 0, errors: [] };
const route = reactive({ fullPath: '/files' });
globalThis.downloadBrowserTest = { calls, route };
const mocks = {
  vue,
  'vue-router': url('export const useRoute=()=>globalThis.downloadBrowserTest.route;'),
  '@/delivery/transferClient': url('export async function runDelivery(source){await source.plan(null);return {blob:new Blob(["test"]),fileName:"test.txt"};}'),
  '@/stores/downloads': url('export const canCancelDownload=()=>true;export const addDownload=()=>{globalThis.downloadBrowserTest.calls.tasks++;return {};};'),
  '@/lib/async': url('export const describeError=String;export const isAbortError=()=>false;export const logError=()=>{};'),
  '@/stores/toast': url('export const useToasts=()=>({success(){},info(){},error(...args){globalThis.downloadBrowserTest.calls.errors.push(args);}});'),
  '@/lib/overlayScroll': url(`import {ref} from ${JSON.stringify(vue)};export const useOverlayScroll=()=>({visible:ref(false),style:ref({}),onThumbPointerDown(){},onThumbPointerMove(){},onThumbPointerUp(){}});`),
};
const cache = new Map();
async function load(file) {
  if (!/\.(vue|ts)$/.test(file)) file += '.ts';
  if (cache.has(file)) return cache.get(file);
  if (file.endsWith('/delivery/download.ts')) return url('export const saveBlob=()=>{globalThis.downloadBrowserTest.calls.saves++;};');
  if (file.endsWith('AppIcon.vue')) return url('export default {render(){return null}};');
  const input = await readFile(new URL(file, import.meta.url), 'utf8');
  let code = transpile(file.endsWith('.vue') ? compileScript(parse(input).descriptor, { id: file, inlineTemplate: true }).content : input);
  for (const match of [...code.matchAll(/from ["']([^"']+)["']/g)]) {
    const name = match[1];
    const dependency = mocks[name] ?? await load(name.startsWith('@/') ? '../src/' + name.slice(2) : new URL(name, new URL(file, import.meta.url)).href);
    code = code.replace(match[0], `from ${JSON.stringify(dependency)}`);
  }
  const result = url(code); cache.set(file, result); return result;
}
// Use a canonical URL so aliases and relative imports share the same store.
const storePath = new URL('../src/stores/downloadBrowser.ts', import.meta.url).href;
const storeURL = await load(storePath);
mocks['@/stores/downloadBrowser'] = storeURL;
const store = await import(storeURL);
const { useDeliveryAction } = await import(await load('../src/delivery/actions.ts'));
const component = (await import(await load('../src/components/AppDownloadBrowserGuide.vue'))).default;
const flush = async () => { await new Promise(resolve => setTimeout(resolve, 0)); await nextTick(); };
const button = text => [...document.querySelectorAll('button')].find(node => node.textContent.trim() === text);
const ua = value => Object.defineProperty(navigator, 'userAgent', { configurable: true, value });
const source = { async plan() { calls.plans++; return {}; } };

test('受限 UA 下载前提示、取消不取数、主动尝试仅放行当前批次', async () => {
  const app = createApp(component); app.mount('#app');
  try {
    const download = useDeliveryAction();
    for (const value of ['MicroMessenger', 'alipayclient', 'AlipayDefined']) {
      ua(value);
      const pending = download.run(source); await flush();
      assert.ok(document.querySelector('[role="dialog"]'));
      assert.equal(calls.tasks, 0); assert.equal(calls.plans, 0);
      assert.equal(document.body.classList.contains('modal-open'), true);
      button('我知道了').click(); assert.equal(await pending, false); await flush();
      assert.equal(document.body.classList.contains('modal-open'), false);
    }
    ua('MicroMessenger');
    const batch = download.runBatch([{ source, silent: true }, { source, silent: true }]); await flush();
    button('仍要尝试直接下载').click(); assert.equal(await batch, true); await flush();
    assert.equal(calls.tasks, 2); assert.equal(calls.plans, 2); assert.equal(calls.saves, 2);
    const again = download.run(source); await flush();
    assert.equal(store.downloadBrowserOpen.value, true);
    route.fullPath = '/share/test'; await flush();
    assert.equal(await again, false); assert.equal(calls.tasks, 2);
    ua('Mozilla/5.0 Chrome');
    assert.equal(await download.run(source), true); assert.equal(calls.tasks, 3);
    assert.deepEqual(calls.errors, []);
  } finally { app.unmount(); store.finishDownloadBrowser(false); }
});
