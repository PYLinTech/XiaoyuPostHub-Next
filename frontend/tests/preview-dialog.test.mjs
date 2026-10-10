import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import test from 'node:test';
import {compileScript,parse} from '@vue/compiler-sfc';
import {createRenderer,h,nextTick,reactive} from 'vue';
import ts from 'typescript';
import {getFileType} from '@eternalheart/vue-file-preview';
const url=c=>`data:text/javascript;base64,${Buffer.from(c).toString('base64')}`, vue=import.meta.resolve('vue');
const mock=url(`import {ref,h} from ${JSON.stringify(vue)};
export class ApiError extends Error{};
export const stopPreviewParser=()=>{};
export const state={prepared:[],downloads:[],released:0,wait:null,renderer:null,streamError:false,fullWait:null};
export const useSite=()=>({state:{theme:'light'}});
export const useDeliveryAction=()=>({busy:ref(false),run:async(source)=>state.downloads.push(source)});
export const preparePreview=async(source,options)=>{state.prepared.push({source,options});if(options.streamingOnly&&state.streamError)throw new Error("流式失败");if(options.forceFull&&state.fullWait)await state.fullWait;if(state.wait)await state.wait;return {mode:options.forceFull?"blob":"stream",url:'blob:test',plan:{mimeType:'text/plain'},release:async()=>state.released++}};
export const loadPreviewLibrary=async()=>({getFileType:({name})=>name.endsWith('.exe')?'unsupported':name.endsWith('.mp4')?'video':'text',FilePreviewContent:{props:{files:Array,headless:Boolean,showDownload:Boolean,showClose:Boolean,mode:String},setup(p){state.renderer=p;return ()=>h('div','预览内容')}}});
export const createRequestGate=()=>{let n=0;return {next:()=>++n,isCurrent:i=>i===n}};export const describeError=e=>e.message;export const logError=()=>{};`);
const state=(await import(mock)).state;
const modal=url(`import {h} from ${JSON.stringify(vue)};export default {props:['open','title'],setup(p,{slots}){return ()=>p.open?h('section',[h('h2',p.title),slots.actions?.(),slots.default?.()]):null}}`);
const button=url(`import {h} from ${JSON.stringify(vue)};export default {setup(p,{slots,attrs}){return ()=>h('button',attrs,slots.default?.())}}`),icon=url(`export default {render(){return null}}`);
let code=compileScript(parse(await readFile(new URL('../src/components/FilePreviewDialog.vue',import.meta.url),'utf8')).descriptor,{id:'preview-test',inlineTemplate:true}).content;
code=ts.transpileModule(code,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ES2022}}).outputText.replace(/from ["']([^"']+)["']/g,(_,n)=>`from ${JSON.stringify(n==='vue'?vue:n.endsWith('AppModal.vue')?modal:n.endsWith('AppButton.vue')?button:n.endsWith('AppIcon.vue')?icon:mock)}`);
const component=(await import(url(code))).default;
const node=text=>({text,children:[],parent:null,attrs:{}});
function remove(el){if(el.parent)el.parent.children.splice(el.parent.children.indexOf(el),1);el.parent=null}
const renderer=createRenderer({createElement:()=>node(''),createText:node,createComment:()=>node(''),setText:(el,t)=>el.text=t,setElementText:(el,t)=>{el.text=t;el.children=[]},parentNode:el=>el.parent,nextSibling:el=>el.parent?.children[el.parent.children.indexOf(el)+1],patchProp:(el,k,prev,v)=>el.attrs[k]=v,insert(el,p,a){if(el.parent)remove(el);el.parent=p;const i=p.children.indexOf(a);p.children.splice(i<0?p.children.length:i,0,el)},remove});
const text=el=>el.text+el.children.map(text).join('');
const flush=async()=>{for(let i=0;i<8;i++)await Promise.resolve();await nextTick()};
function mount(name){Object.assign(state,{prepared:[],downloads:[],released:0,wait:null,renderer:null,streamError:false,fullWait:null});const props=reactive({open:true,fileName:name,source:{id:'preview'},downloadSource:{id:'download'}}),root=node(''),app=renderer.createApp({render:()=>h(component,props)});app.mount(root);return {props,root,app}}
test('未知格式不取文件，下载使用独立下载来源',async()=>{const {props,root,app}=mount('安装包.exe');try{await flush();assert.match(text(root),/该格式暂不支持预览/);assert.equal(state.prepared.length,0);function find(el){return el.attrs.onClick&&text(el).trim()==='下载'?el:el.children.map(find).find(Boolean)}await find(root).attrs.onClick();assert.equal(state.downloads[0],props.downloadSource)}finally{app.unmount()}});
test('支持格式使用 headless 内容，关闭释放会话',async()=>{const {props,root,app}=mount('说明.txt');try{await flush();assert.match(text(root),/预览内容/);assert.equal(state.renderer.headless,true);assert.equal(state.renderer.showDownload,false);assert.equal(state.renderer.showClose,false);props.open=false;await flush();assert.equal(state.released,1);assert.equal(text(root),'')}finally{app.unmount()}});
test('关闭准备中的预览会取消，迟到资源仍被释放',async()=>{const {props,app}=mount('说明.txt');let resolve;state.wait=new Promise(r=>resolve=r);try{await flush();props.open=false;await flush();assert.equal(state.prepared[0].options.signal.aborted,true);resolve();await flush();assert.equal(state.released,1)}finally{app.unmount();state.wait=null}});
test('发布包识别 Office、PDF、ZIP、代码与未知类型',()=>{for(const [name,type] of [['a.docx','docx'],['a.xlsx','xlsx'],['a.pdf','pdf'],['a.zip','zip'],['a.ts','text'],['a.exe','unsupported']])assert.equal(getFileType({name,type:'',url:''}),type)});

function errorTarget(root){return root.attrs.onErrorCapture?root:root.children.map(errorTarget).find(Boolean)}
globalThis.location={href:'https://preview.test/'};
globalThis.HTMLMediaElement=class{constructor(src){this.currentSrc=src;this.src=src}};
test('播放流失败仅回落一次，完整获取期间显示兼容提示，关闭后取消并释放迟到资源',async()=>{
  const {props,root,app}=mount('视频.mp4');let resolve;state.fullWait=new Promise(r=>resolve=r);
  try{
    await flush();
    const listener=errorTarget(root).attrs.onErrorCapture;
    listener({target:new HTMLMediaElement('blob:test')});
    await flush();
    assert.match(text(root),/流式预览失败，正在自动尝试兼容方法（需完整获取文件后再预览）/);
    assert.equal(state.prepared.length,2);
    assert.equal(state.prepared[1].options.forceFull,true);
    listener({target:new HTMLMediaElement('blob:test')});
    assert.equal(state.prepared.length,2);
    props.open=false;await flush();
    assert.equal(state.prepared[1].options.signal.aborted,true);
    resolve();await flush();
    assert.equal(state.released,2);
  }finally{app.unmount();state.fullWait=null}
});
test('流式准备失败也自动回落，完整预览再次播放失败不循环重取',async()=>{
  const {root,app}=mount('视频.mp4');state.streamError=true;
  try{
    await flush();await flush();
    assert.equal(state.prepared.length,2);
    assert.equal(state.prepared[0].options.streamingOnly,true);
    assert.equal(state.prepared[1].options.forceFull,true);
    assert.match(text(root),/预览内容/);
    errorTarget(root).attrs.onErrorCapture({target:new HTMLMediaElement('blob:test')});
    await flush();assert.equal(state.prepared.length,2);
  }finally{app.unmount();state.streamError=false}
});

test('播放器自身的非原生错误也触发兼容取数',async()=>{
  const {root,app}=mount('视频.mp4');
  try{
    await flush();
    errorTarget(root).attrs.onXphPreviewError({detail:{url:'blob:test'}});
    await flush();await flush();
    assert.equal(state.prepared.length,2);
    assert.equal(state.prepared[1].options.forceFull,true);
  }finally{app.unmount()}
});
