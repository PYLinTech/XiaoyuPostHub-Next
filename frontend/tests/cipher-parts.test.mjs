import assert from 'node:assert/strict';
import test from 'node:test';
import {readFile} from 'node:fs/promises';
import ts from 'typescript';
const code=ts.transpileModule(await readFile(new URL('../src/delivery/cipherParts.ts',import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ES2022}}).outputText;
const {readCipherParts}=await import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`);
test('缺卷、重叠和非法偏移在请求之前拒绝',async()=>{
  let calls=0;
  for(const parts of [
    [{url:'a',offset:0,size:2},{url:'b',offset:3,size:2}],
    [{url:'a',offset:0,size:3},{url:'b',offset:2,size:3}],
    [{url:'a',offset:-1,size:6}],
  ]) await assert.rejects(readCipherParts(parts,0,4,async()=>{calls++;return new Uint8Array(5)}),/分卷清单/);
  assert.equal(calls,0);
});
test('某卷失败会取消其他在途读取并保留原始错误',async()=>{
  let cancelled=0;
  await assert.rejects(readCipherParts([{url:'bad',offset:0,size:1},{url:'slow',offset:1,size:1}],0,1,
    async(url,_start,_end,signal)=>{
      if(url==='bad')throw new Error('卷已失效');
      return new Promise((_,reject)=>signal.addEventListener('abort',()=>{cancelled++;reject(signal.reason)},{once:true}));
    }),/卷已失效/);
  assert.equal(cancelled,1);
});
test('大量小卷限制并行请求，按逻辑顺序重组',async()=>{
  let active=0,max=0;
  const parts=Array.from({length:25},(_,i)=>({url:String(i),offset:i,size:1}));
  const bytes=await readCipherParts(parts.reverse(),0,24,async(url)=>{
    active++;max=Math.max(max,active);await new Promise(r=>setTimeout(r,1));active--;return Uint8Array.of(Number(url));
  });
  assert.ok(max<=4);assert.deepEqual(bytes,Uint8Array.from({length:25},(_,i)=>i));
});
