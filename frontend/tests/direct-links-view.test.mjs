import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import ts from 'typescript';
import { JSDOM } from 'jsdom';
import { compileScript, parse } from '@vue/compiler-sfc';

const dom = new JSDOM('<div id="app"></div><div id="topbar"></div>', {url:'http://localhost/'});
for(const key of ['window','document','HTMLElement','SVGElement','Element','Node']) globalThis[key]=key==='window'?dom.window:dom.window[key];
globalThis.requestAnimationFrame = callback => setTimeout(callback, 0);
globalThis.cancelAnimationFrame = clearTimeout;
const {createApp,nextTick,ref}=await import('vue');
const vue=import.meta.resolve('vue');
const url=code=>`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`;
const transpile=code=>ts.transpileModule(code,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ES2022}}).outputText;
const source=async file=>readFile(new URL(file,import.meta.url),'utf8');
let links=Array.from({length:50},(_,i)=>({id:String(i),checksum:'a'.repeat(64),sourceName:`file-${i}.bin`,sourcePath:`/old/file-${i}.bin`,hits:i===0?5:0,maxHits:i===0?5:0,disabled:false,targetAvailable:true,createdAt:1,updatedAt:1}));
const updates=[],navigations=[];
const enabled=ref(true);
globalThis.linkViewTest={
 api:{list:async(limit,offset)=>({items:links.slice(offset,offset+limit)}),target:async()=>({path:'/new/renamed.bin'}),create:async input=>{updates.push(input);return {link:links[0]};},update:async(_,input)=>{updates.push(input);},remove:async id=>{links=links.filter(x=>x.id!==id);}},
 session:{state:{authenticated:true},directLinksEnabled:enabled,can:()=>true},
 router:{push:async value=>{navigations.push(value);},replace:async()=>{}},
};
const client=url('export class ApiError extends Error{};');
const asyncLib=url(transpile(await source('../src/lib/async.ts')).replace('"vue"',JSON.stringify(vue)).replace('"@/api/client"',JSON.stringify(client)));
const mocks={
 vue,
 'vue-router':url('export const useRoute=()=>({query:{}});export const useRouter=()=>globalThis.linkViewTest.router;'),
 '@/api/endpoints':url('export const directLinkApi=globalThis.linkViewTest.api;export const directLinkUrl=id=>"http://localhost/d/"+id;'),
 '@/stores/session':url('export const useSession=()=>globalThis.linkViewTest.session;'),
 '@/stores/shell':url(`import {ref} from ${JSON.stringify(vue)};export const topbarSlot=ref(document.querySelector('#topbar'));`),
 '@/stores/toast':url('export const useToasts=()=>({success(){},error(){}});'),
 '@/lib/async':asyncLib,
 '@/lib/overlayScroll':url(`import {ref} from ${JSON.stringify(vue)};export const useOverlayScroll=()=>({visible:ref(false),style:ref({}),onThumbPointerDown(){},onThumbPointerMove(){},onThumbPointerUp(){}});`),
};
const blank=url('export default {render(){return null}};');
const cache=new Map();
async function load(file){
 if (!/\.(vue|ts)$/.test(file)) file += ".ts";
 if(cache.has(file))return cache.get(file);
 if(file.endsWith('AppDateTimePicker.vue')||file.endsWith('AppSelect.vue')) return blank;
 const input=await source(file);
 let code=file.endsWith('.vue')?compileScript(parse(input).descriptor,{id:file,inlineTemplate:true}).content:input;
 code=transpile(code).replaceAll('import.meta.env.DEV','false');
 for(const match of [...code.matchAll(/from ["']([^"']+)["']/g)]){
  const name=match[1];
  const dependency=mocks[name]??await load(name.startsWith('@/')?'../src/'+name.slice(2):new URL(name,new URL(file,import.meta.url)).href);
  code=code.replace(match[0],`from ${JSON.stringify(dependency)}`);
 }
 const result=url(code);cache.set(file,result);return result;
}
const component=(await import(await load('../src/views/DirectLinksView.vue'))).default;
const flush=async()=>{await new Promise(resolve=>setTimeout(resolve,0));await nextTick();};
const button=(root,text)=>[...root.querySelectorAll('button')].find(x=>x.textContent.trim()===text);

test('直链页面支持空页返回、耗尽状态、文件定位和严格表单校验',async()=>{
 const app=createApp(component);app.mount('#app');
 try{
  await flush();
  assert.match(document.querySelector('#app').textContent,/次数已耗尽/);
  assert.match(document.querySelector('.pagination').textContent,/已显示 50 条/);
  button(document,'下一页').click();await flush();
  assert.match(document.querySelector('#app').textContent,/没有更多直链/);
  assert.ok(button(document,'上一页'));
  assert.equal(button(document,'上一页').disabled,false);
  button(document,'上一页').click();await flush();
  document.querySelector('.link-button').click();await flush();
  assert.deepEqual(navigations.at(-1),{name:'files',params:{path:['new']},query:{select:'/new/renamed.bin'}});
  button(document,'新建直链').click();await flush();
  const form=document.querySelector('#direct-link-form');
  const path=form.querySelector('[aria-label="目标路径"]');path.value='/new.bin';path.dispatchEvent(new dom.window.Event('input',{bubbles:true}));
  const count=form.querySelector('[aria-label="次数上限"]');count.value='1.5';count.dispatchEvent(new dom.window.Event('input',{bubbles:true}));
  form.dispatchEvent(new dom.window.Event('submit',{bubbles:true,cancelable:true}));await flush();
  assert.equal(updates.length,0);assert.match(form.textContent,/非负整数/);
  count.value='2';count.dispatchEvent(new dom.window.Event('input',{bubbles:true}));
  form.dispatchEvent(new dom.window.Event('submit',{bubbles:true,cancelable:true}));await flush();
  assert.equal(updates.length,1);assert.equal(updates[0].maxHits,2);
  enabled.value=false;await flush();assert.equal(document.querySelector('#topbar').textContent,'');
 }finally{app.unmount();dom.window.close();delete globalThis.linkViewTest;}
});
