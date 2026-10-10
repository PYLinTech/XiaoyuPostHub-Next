import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { JSDOM } from 'jsdom';
import { compileScript, parse } from '@vue/compiler-sfc';
import ts from 'typescript';

const window = new JSDOM('<div id="app"></div>').window;
for (const key of ['window', 'document', 'HTMLElement', 'SVGElement', 'Element', 'Node']) globalThis[key] = key === 'window' ? window : window[key];
globalThis.requestAnimationFrame = callback => setTimeout(callback, 0);
const { createApp, h, nextTick, reactive } = await import('vue');
const vue = import.meta.resolve('vue');
const url = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`;
const icon = url('export default {render(){return null}}');
const scroll = url(`import {ref} from ${JSON.stringify(vue)};export const useOverlayScroll=()=>({visible:ref(false),style:ref({}),onThumbPointerDown(){},onThumbPointerMove(){},onThumbPointerUp(){}})`);
let code = compileScript(parse(await readFile(new URL('../src/components/ui/AppModal.vue', import.meta.url), 'utf8')).descriptor, { id: 'modal-test', inlineTemplate: true }).content;
code = ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText
  .replace(/from ["']([^"']+)["']/g, (_, name) => `from ${JSON.stringify(name === 'vue' ? vue : name.endsWith('AppIcon.vue') ? icon : scroll)}`);
const component = (await import(url(code))).default;

test('嵌套弹窗标题唯一，关闭与卸载仅释放自身滚动锁，Esc 只关闭顶层', async () => {
  const state = reactive({ outer: true, inner: false, idle: true });
  const app = createApp({ render: () => [
    h(component, { open: state.outer, title: '外层', onClose: () => state.outer = false }),
    h(component, { open: state.inner, title: '内层', panelClass: 'test-preview', onClose: () => state.inner = false }),
    state.idle ? h(component, { open: false, title: '未打开' }) : null,
  ] });
  app.mount('#app');
  try {
    await nextTick(); await nextTick();
    assert.equal(document.body.classList.contains('modal-open'), true);
    state.idle = false;
    await nextTick();
    assert.equal(document.body.classList.contains('modal-open'), true);
    state.inner = true;
    await nextTick(); await nextTick();
    const titles = [...document.querySelectorAll('.modal__title')].map(el => el.id);
    assert.equal(new Set(titles).size, 2);
    assert.ok(document.querySelector('.modal__panel.test-preview'));
    document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape' }));
    await nextTick();
    assert.equal(state.inner, false);
    assert.equal(state.outer, true);
    assert.equal(document.body.classList.contains('modal-open'), true);
    document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape' }));
    await nextTick();
    assert.equal(state.outer, false);
    assert.equal(document.body.classList.contains('modal-open'), false);
  } finally {
    app.unmount();
    window.close();
  }
});
