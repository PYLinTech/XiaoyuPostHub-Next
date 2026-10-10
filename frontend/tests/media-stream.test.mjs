import assert from 'node:assert/strict';
import test from 'node:test';
import { build } from 'esbuild';
import { resolve } from 'node:path';
const handlers=new Map();
globalThis.self={location:{origin:'https://example.test'},addEventListener:(name,fn)=>handlers.set(name,fn)};
const built=await build({entryPoints:[resolve('src/sw.ts')],bundle:true,write:false,format:'esm',platform:'browser',alias:{'@':resolve('src')},logLevel:'silent'});
await import(`data:text/javascript;base64,${Buffer.from(built.outputFiles[0].text).toString('base64')}`);
const plain=Uint8Array.from({length:512*25+71},(_,i)=>i%251);
const dek=crypto.getRandomValues(new Uint8Array(32));
const key=await crypto.subtle.importKey('raw',dek,'AES-GCM',false,['encrypt']);
const header=new Uint8Array(64);header.set(new TextEncoder().encode('XPHCRPT1'));header[8]=1;header[9]=9;
new DataView(header.buffer).setBigUint64(28,BigInt(plain.length),true);
const parts=[header];
for(let i=0;i<Math.ceil(plain.length/512);i++) {
  const iv=new Uint8Array(12);new DataView(iv.buffer).setBigUint64(4,BigInt(i));
  parts.push(new Uint8Array(await crypto.subtle.encrypt({name:'AES-GCM',iv,additionalData:header.slice(0,40)},key,plain.slice(i*512,(i+1)*512))));
}
const cipher=new Uint8Array(parts.reduce((n,p)=>n+p.length,0));let offset=0;for(const part of parts){cipher.set(part,offset);offset+=part.length;}
let reads=[];
function mockFetch(volumes) {
  reads=[];
  globalThis.fetch=async(_url,{headers,signal})=>{
    signal?.throwIfAborted();
    const [,from,to]=/^bytes=(\d+)-(\d+)$/.exec(headers.Range);reads.push([Number(from),Number(to)]);
    return new Response((volumes?.get(_url) || cipher).slice(Number(from),Number(to)+1),{status:206});
  };
}
async function register(cipherParts, volumes) {
  mockFetch(volumes);
  return new Promise((resolve,reject)=>handlers.get('message')({data:{type:'xph:register',cipherParts,cipherUrl:'https://storage.test/cipher',dek:Buffer.from(dek).toString('base64'),mimeType:'video/mp4',fileName:'movie.mp4'},ports:[{postMessage:data=>data.ok?resolve(data.id):reject(new Error(data.error))}],waitUntil(){}}));
}
async function response(id,range,options={}) {
  let promise;
  handlers.get('fetch')({request:new Request(`https://example.test/__xph/${id}`,{...options,headers:range?{Range:range}:{}}),respondWith:p=>promise=p});
  return promise;
}
function revoke(id){handlers.get('message')({data:{type:'xph:revoke',id},ports:[]});}
test('开放式 Range 立即响应，首批可解密播放，未读取部分不下载',async()=>{
  const id=await register();try{
    const result=await response(id,'bytes=0-');
    assert.equal(result.status,206);assert.equal(result.headers.get('Content-Length'),String(plain.length));
    assert.equal(reads.length,1); // 只有文件头，响应无需等待内容。
    const reader=result.body.getReader();
    const first=await reader.read();
    assert.deepEqual(first.value,plain.slice(0,512));
    assert.equal(reads.length,2);
    await reader.cancel();
    assert.equal(reads.at(-1)[1],64+8*528-1);
  }finally{revoke(id);}
});
test('跨加密块拖动、尾部元数据、完整流均返回正确明文',async()=>{
  const id=await register();try{
    for(const [range,start,end] of [['bytes=503-1061',503,1062],['bytes=-31',plain.length-31,plain.length],[undefined,0,plain.length]]){
      const result=await response(id,range);
      assert.deepEqual(new Uint8Array(await result.arrayBuffer()),plain.slice(start,end));
    }
    const head=await response(id,undefined,{method:'HEAD'});
    assert.equal(head.headers.get('Content-Length'),String(plain.length));
    assert.equal(await head.text(),'');
    for(const range of ['bytes=0junk-10','bytes=0-10junk','bytes=-','bytes=999999-']) assert.equal((await response(id,range)).status,416);
  }finally{revoke(id);}
});
test('关闭预览终止正在读取的密文，后续虚拟地址不可访问',async()=>{
  const id=await register();
  let began,aborted=false;const started=new Promise(r=>began=r);
  globalThis.fetch=async(_url,{signal})=>new Promise((_,reject)=>{
    signal.addEventListener('abort',()=>{aborted=true;reject(signal.reason);},{once:true});began();
  });
  const result=await response(id,'bytes=0-');const reader=result.body.getReader();
  const pending=reader.read();await started;revoke(id);
  await assert.rejects(pending,{name:'AbortError'});assert.equal(aborted,true);
  assert.equal((await response(id,'bytes=0-')).status,404);
});
test('上游忽略 Range 时不读取整份密文',async()=>{
  const id=await register();try{
    let cancelled=false;
    globalThis.fetch=async()=>new Response(new ReadableStream({cancel(){cancelled=true;}}),{status:200});
    const result=await response(id,'bytes=0-');
    await assert.rejects(result.body.getReader().read(),/取密文失败/);
    assert.equal(cancelled,true);
  }finally{revoke(id);}
});

test('真实 AES 分卷预览：跨卷读取、尾部拖动、整流重组均与原文件一致',async()=>{
  const cuts=[0,64+3*528,64+11*528,64+19*528,cipher.length];
  const volumes=new Map(), manifest=[];
  for(let i=0;i<cuts.length-1;i++){
    const url=`https://storage.test/part${i}`;
    volumes.set(url,cipher.slice(cuts[i],cuts[i+1]));manifest.push({url,offset:cuts[i],size:cuts[i+1]-cuts[i]});
  }
  const id=await register(manifest.reverse(),volumes);
  try{
    for(const [range,from,to] of [['bytes=1400-6100',1400,6101],['bytes=-900',plain.length-900,plain.length],[undefined,0,plain.length]]){
      const result=await response(id,range);
      assert.deepEqual(new Uint8Array(await result.arrayBuffer()),plain.slice(from,to));
    }
  }finally{revoke(id)}
});

test('注册期间关闭预览会取消文件头读取，不留下迟到会话', async () => {
  const id = 'cancel-pending-preview';
  let started;
  const reading = new Promise(resolve => { started = resolve; });
  globalThis.fetch = async (_url, { signal }) => {
    started();
    return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
  };
  let result;
  let work;
  handlers.get('message')({
    data: { type: 'xph:register', id, cipherUrl: 'https://storage.test/cipher', dek: Buffer.from(dek).toString('base64'), mimeType: 'video/mp4', fileName: 'movie.mp4' },
    ports: [{ postMessage: data => { result = data; } }], waitUntil(promise) { work = promise; },
  });
  await reading;
  revoke(id);
  await work;
  assert.equal(result.ok, false);
  assert.equal((await response(id)).status, 404);
});

test('完整批次尚未下载时首个认证块即可播放，取消会终止等待中的响应体',async()=>{
  const id=await register();let cancelled=false;
  try{
    globalThis.fetch=async(_url,{headers})=>{
      const [,begin,end]=/^bytes=(\d+)-(\d+)$/.exec(headers.Range);
      return new Response(new ReadableStream({
        start(output){output.enqueue(cipher.slice(+begin,+begin+528));},
        cancel(){cancelled=true},
      }),{status:206,headers:{'Content-Range':`bytes ${begin}-${end}/${cipher.length}`}});
    };
    const result=await response(id,'bytes=0-');const reader=result.body.getReader();
    const first=await Promise.race([reader.read(),new Promise((_,reject)=>setTimeout(()=>reject(Error('首块等待了整个批次')),500))]);
    assert.deepEqual(first.value,plain.slice(0,512));
    await reader.cancel();await new Promise(r=>setTimeout(r,0));
    assert.equal(cancelled,true);
  }finally{revoke(id)}
});

test('不同明文小区间共享完整认证块，后续命中缓存不会再请求密文',async()=>{
  const id=await register();try{
    const before=reads.length;
    const [a,b]=await Promise.all([response(id,'bytes=10-30'),response(id,'bytes=100-120')]);
    const [av,bv]=await Promise.all([a.arrayBuffer(),b.arrayBuffer()]);
    assert.deepEqual(new Uint8Array(av),plain.slice(10,31));assert.deepEqual(new Uint8Array(bv),plain.slice(100,121));
    assert.equal(reads.length-before,1);
    const cached=await response(id,'bytes=200-250');assert.deepEqual(new Uint8Array(await cached.arrayBuffer()),plain.slice(200,251));
    assert.equal(reads.length-before,1);
  }finally{revoke(id)}
});
