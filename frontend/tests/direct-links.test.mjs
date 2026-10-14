import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { JSDOM } from 'jsdom';
import { compileScript, parse } from '@vue/compiler-sfc';
import ts from 'typescript';

const window = new JSDOM('<div id="app"></div>').window;
for (const key of ['window', 'document', 'HTMLElement', 'SVGElement', 'Element', 'Node']) {
  globalThis[key] = key === 'window' ? window : window[key];
}
const { createApp, nextTick } = await import('vue');
const vue = import.meta.resolve('vue');
const url = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`;
const transpile = code => ts.transpileModule(code, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 },
}).outputText;
const source = path => readFile(new URL(path, import.meta.url), 'utf8');
const types = url(transpile(await source('../src/api/types.ts')));
globalThis.directLinkPublicState = { siteName: 'test', directLinkEnabled: false };
const api = url(`export const setupApi={state:async()=>globalThis.directLinkStateLoader ? globalThis.directLinkStateLoader() : ({...globalThis.directLinkPublicState})}; export const authApi={};`);
const client = url('export class ApiError extends Error{}; export const getToken=()=>""; export const setToken=()=>{}; export const setUnauthorizedHandler=()=>{};');
const sessionUrl = url(transpile(await source('../src/stores/session.ts')).replace(/from ["']([^"']+)["']/g,
  (_, name) => `from ${JSON.stringify({ vue, '@/api/client': client, '@/api/endpoints': api, '@/api/types': types }[name])}`));
const { useSession } = await import(sessionUrl);
const empty = url('export default {render(){return null}}');
const imports = {
  vue,
  'vue-router': url('export const useRoute=()=>({path:"/files"});export const useRouter=()=>({push:async()=>{}});'),
  '@/assets/logo.svg': url('export default "logo.svg";'),
  '@/api/types': types,
  '@/lib/format': url('export const formatBytes=String;'),
  '@/lib/overlayScroll': url(`import {ref} from ${JSON.stringify(vue)};export const useOverlayScroll=()=>({visible:ref(false),style:ref({})});`),
  '@/stores/session': sessionUrl,
  '@/stores/site': url('export const useSite=()=>({state:{theme:"light"},toggleTheme(){}});'),
  '@/stores/toast': url('export const useToasts=()=>({success(){}});'),
  '@/stores/mail': url('export const useMailShell=()=>({state:{unreadInbox:0}});'),
};
const descriptor = parse(await source('../src/components/AppSideNav.vue')).descriptor;
const componentUrl = url(transpile(compileScript(descriptor, { id: 'direct-link-nav', inlineTemplate: true }).content)
  .replace(/from ["']([^"']+)["']/g, (_, name) => `from ${JSON.stringify(imports[name] ?? empty)}`));
const component = (await import(componentUrl)).default;

test('公开总开关控制侧栏入口，关闭后即使已登录也不显示直链', async () => {
  const session = useSession();
  session.state.authenticated = true;
  session.state.user = { account: 'owner' };
  const app = createApp(component, { open: true });
  app.mount('#app');
  try {
    await session.reloadSiteInfo();
    await nextTick();
    assert.equal(document.body.textContent.includes('直链'), false);
    globalThis.directLinkPublicState.directLinkEnabled = true;
    await session.reloadSiteInfo();
    await nextTick();
    assert.equal(document.body.textContent.includes('直链'), true);
    // 创建权限为 0 仍可管理已有直链；系统开关关闭则全部隐藏。
    assert.equal(session.state.permissions, 0);
    globalThis.directLinkPublicState.directLinkEnabled = false;
    await session.reloadSiteInfo();
    await nextTick();
    assert.equal(document.body.textContent.includes('直链'), false);
    const pending = [];
    globalThis.directLinkStateLoader = () => new Promise(resolve => pending.push(resolve));
    const older = session.reloadSiteInfo();
    const newer = session.reloadSiteInfo();
    pending[1]({ siteName: 'test', directLinkEnabled: false });
    await newer;
    pending[0]({ siteName: 'test', directLinkEnabled: true });
    await older;
    await nextTick();
    assert.equal(session.directLinksEnabled.value, false, 'stale configuration must not re-enable links');
  } finally {
    app.unmount();
    window.close();
    delete globalThis.directLinkPublicState;
    delete globalThis.directLinkStateLoader;
  }
});
