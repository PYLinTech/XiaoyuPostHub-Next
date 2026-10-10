import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import ts from 'typescript';
const url = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`;
const transpile = code => ts.transpileModule(code, {compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ES2022}}).outputText;
const errors = url(`export const packError=e=>({name:e.name,message:e.message}); export const unpackError=e=>Object.assign(new Error(e.message),{name:e.name});`);
const code = transpile(await readFile(new URL('../src/lib/previewParser.ts',import.meta.url),'utf8'))
  .replace('"@/delivery/transferProtocol"',JSON.stringify(errors))
  .replace('new URL("./previewParser.worker.ts", import.meta.url)', '"preview-worker"');
const parser = await import(url(code));
class FakeWorker {
  static instances=[];
  constructor() { FakeWorker.instances.push(this); this.messages=[]; this.terminated=false; }
  postMessage(message) { structuredClone(message); this.messages.push(message); }
  terminate() { this.terminated=true; }
}
globalThis.Worker=FakeWorker;
test('窗口关闭终止线程，旧渲染器不能启动新线程，新预览重新创建线程', async () => {
  const session=parser.createPreviewParser();
  const first=session.parse('docx',new ArrayBuffer(4));
  const cancelled=assert.rejects(first,{name:'AbortError'});
  const worker=FakeWorker.instances.at(-1);
  parser.stopPreviewParser();
  await cancelled;
  assert.equal(worker.terminated,true);
  const count=FakeWorker.instances.length;
  await assert.rejects(session.parse('xlsx',new ArrayBuffer(4)),{name:'AbortError'});
  assert.equal(FakeWorker.instances.length,count);
  const next=parser.createPreviewParser().highlight('x',{lang:'text',theme:'github-light',transformers:[{line(){}}]});
  const fresh=FakeWorker.instances.at(-1);
  assert.notEqual(fresh,worker);
  const message=fresh.messages[0];
  assert.equal(message.value.lineNumbers,true);
  assert.equal(message.value.options.transformers,undefined);
  fresh.onmessage({data:{id:message.id,value:'highlighted'}});
  assert.equal(await next,'highlighted');
  parser.stopPreviewParser();
});
test('全局预览只保留最后一个请求，来源保持原对象，关闭清空', async () => {
  const storeCode=transpile(await readFile(new URL('../src/stores/preview.ts',import.meta.url),'utf8')).replace('"vue"',JSON.stringify(import.meta.resolve('vue')));
  const store=await import(url(storeCode));
  const source={plan(){}};
  store.openFilePreview({fileName:'一.pdf',source,downloadSource:source});
  store.openFilePreview({fileName:'二.pdf',source,downloadSource:source});
  assert.equal(store.previewRequest.value.fileName,'二.pdf');
  assert.equal(store.previewRequest.value.source,source);
  store.closeFilePreview();
  assert.equal(store.previewRequest.value,null);
});
