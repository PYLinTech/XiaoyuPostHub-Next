import assert from 'node:assert/strict';
import test from 'node:test';
import {build} from 'esbuild';
import {resolve} from 'node:path';
import {Output,BufferTarget,Mp4OutputFormat,EncodedAudioPacketSource,EncodedPacket,Input,BlobSource,ALL_FORMATS,EncodedPacketSink} from 'mediabunny';

async function module(path) {
  const result = await build({entryPoints:[resolve(path)],bundle:true,write:false,format:'esm',platform:'node',external:['mediabunny'],logLevel:'silent'});
  const code = result.outputFiles[0].text.replaceAll('"mediabunny"',JSON.stringify(import.meta.resolve('mediabunny')));
  return import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`);
}
const {createMediaWindowScheduler} = await module('src/lib/mediaWindowScheduler.ts');
const {MediaRangeSource} = await module('src/lib/mediaRangeSource.ts');
const {mediaKind,mediaMimeType} = await module('src/lib/mediaFormats.ts');
const {mediaSourceConstructor,supportsMediaSourceType} = await module('src/lib/mediaSourceSupport.ts');
const {inspectMedia,remuxMediaSlice} = await module('src/lib/mediaRemux.ts');
const flush = async () => {for(let i=0;i<12;i++)await Promise.resolve()};

test('没有普通 MSE 的 Safari 可选择 ManagedMediaSource',()=>{
  const regular=globalThis.MediaSource,managed=globalThis.ManagedMediaSource;
  try{
    globalThis.MediaSource=undefined;
    globalThis.ManagedMediaSource=class{static isTypeSupported(mime){return mime==='audio/mp4'}};
    assert.equal(mediaSourceConstructor(),globalThis.ManagedMediaSource);
    assert.equal(supportsMediaSourceType('audio/mp4'),true);
    assert.equal(supportsMediaSourceType('video/unknown'),false);
  }finally{
    if(regular===undefined)delete globalThis.MediaSource;else globalThis.MediaSource=regular;
    if(managed===undefined)delete globalThis.ManagedMediaSource;else globalThis.ManagedMediaSource=managed;
  }
});

test('主流扩展名、明确 MIME 和代码 ts 的分类保持准确',()=>{
  for(const ext of ['mp4','m4v','mov','webm','mkv','3gp','mts','m2ts'])assert.equal(mediaKind('a.'+ext),'video');
  for(const ext of ['mp3','wav','wave','m4a','flac','aac','adts','ogg','oga','opus','spx'])assert.equal(mediaKind('a.'+ext),'audio');
  assert.equal(mediaKind('a.ts'),null);
  assert.equal(mediaKind('a.ts','video/mp2t'),'video');
  assert.equal(mediaKind('a.ogg','video/ogg'),'video');
  assert.equal(mediaKind('a.webm','audio/webm'),'audio');
  assert.equal(mediaMimeType('a.opus','application/octet-stream'),'audio/ogg');
});

test('首帧先完成，再并发预取前后窗口，后台慢片不阻挡目标',async()=>{
  const abort=new AbortController(),calls=[],loaded=new Set();let first;
  const blocked=new Promise(resolve=>first=resolve);
  const scheduler=createMediaWindowScheduler({signal:abort.signal,duration:()=>100,loaded:i=>loaded.has(i),error:assert.fail,
    async load(i,signal,target){calls.push({i,target});if(target!==null)await blocked;signal.throwIfAborted();loaded.add(i)} });
  const work=scheduler.run(20,true);await flush();
  assert.deepEqual(calls,[{i:5,target:20}]);
  first();await work;
  assert.deepEqual(calls.slice(1,5).map(c=>c.i),[6,4,7,3]);
  assert.ok(calls.every((c,index)=>index===0||c.target===null));
  scheduler.stop();
});

test('跳转终止旧窗口，取消不会触发完整加载兜底',async()=>{
  const root=new AbortController(),calls=[];let oldSignal;let failures=0;
  const scheduler=createMediaWindowScheduler({signal:root.signal,duration:()=>100,loaded:()=>false,error:()=>failures++,
    async load(i,signal,target){calls.push(i);if(i===0){oldSignal=signal;await new Promise((_,reject)=>signal.addEventListener('abort',()=>reject(signal.reason),{once:true}));}if(target===null)return;} });
  const initial=scheduler.run(0,true);await flush();const seek=scheduler.run(40,true);await seek;await initial;
  assert.equal(oldSignal.aborted,true);assert.equal(failures,0);assert.equal(calls[1],10);
  scheduler.stop();
});

test('停止调度后，迟到的媒体事件不能重新启动网络任务',async()=>{
  let calls=0;
  const scheduler=createMediaWindowScheduler({signal:new AbortController().signal,duration:()=>100,loaded:()=>false,
    load:async()=>{calls++},error:assert.fail});
  scheduler.stop();await scheduler.run(0,true);await scheduler.run(40);
  assert.equal(calls,0);
});

test('按页 Range 缓存、重复读取去重，并限制总体网络并发',async()=>{
  const original=globalThis.fetch,bytes=Uint8Array.from({length:256*1024*8},(_,i)=>i%251);let reads=0,active=0,max=0;
  globalThis.fetch=async(_url,{method,headers,signal})=>{
    signal.throwIfAborted();if(method==='HEAD')return new Response(null,{headers:{'Content-Length':String(bytes.length)}});
    reads++;active++;max=Math.max(max,active);await new Promise(r=>setTimeout(r,2));active--;signal.throwIfAborted();
    const [,start,end]=/^bytes=(\d+)-(\d+)$/.exec(headers.Range);
    return new Response(bytes.slice(+start,+end+1),{status:206,headers:{'Content-Range':`bytes ${start}-${end}/${bytes.length}`}});
  };
  try{
    const source=new MediaRangeSource('https://test/virtual'),signal=new AbortController().signal;await source.open(signal);
    const [a,b]=await Promise.all([source.read(10,100,signal),source.read(40,120,signal)]);
    assert.deepEqual(a,bytes.slice(10,100));assert.deepEqual(b,bytes.slice(40,120));assert.equal(reads,1);
    await Promise.all(Array.from({length:7},(_,i)=>source.read((i+1)*256*1024,(i+1)*256*1024+20,signal)));
    assert.ok(max<=4);assert.equal(reads,8);source.clear();
  }finally{globalThis.fetch=original}
});

test('上游忽略 Range 时拒绝响应，不读取完整视频',async()=>{
  const original=globalThis.fetch;let consumed=false,cancelled=false;
  globalThis.fetch=async(_url,{method})=>method==='HEAD'?new Response(null,{headers:{'Content-Length':'100'}}):{
    status:200,headers:new Headers(),body:{cancel:async()=>cancelled=true},arrayBuffer:async()=>{consumed=true;return new ArrayBuffer(100)},
  };
  try{
    const source=new MediaRangeSource('https://test/virtual'),signal=new AbortController().signal;await source.open(signal);
    await assert.rejects(source.read(0,10,signal),/分片区间/);assert.equal(consumed,false);assert.equal(cancelled,true);
  }finally{globalThis.fetch=original}
});

test('时长探测预算按实际网络页计算，不允许用小头部请求扫描整份文件',async()=>{
  const original=globalThis.fetch;let reads=0;
  globalThis.fetch=async(_url,{method})=>{
    if(method==='HEAD')return new Response(null,{headers:{'Content-Length':String(10*1024*1024)}});
    reads++;throw Error('不应读取超出预算的网络页');
  };
  let input;
  try{
    const source=new MediaRangeSource('https://test/virtual'),signal=new AbortController().signal;await source.open(signal);
    input=source.input(signal,100);
    await assert.rejects(input.getFormat(),/读取上限/);assert.equal(reads,0);
  }finally{input?.dispose();globalThis.fetch=original}
});

test('普通 M4A 前端重新封装：保留绝对时间及编码包，纯音频 MIME 正确',async()=>{
  const originalFetch=globalThis.fetch,originalMse=globalThis.MediaSource;
  const output=new Output({target:new BufferTarget(),format:new Mp4OutputFormat({fastStart:false})});
  const writer=new EncodedAudioPacketSource('aac');output.addAudioTrack(writer);await output.start();
  const config={codec:'mp4a.40.2',sampleRate:48000,numberOfChannels:2,description:new Uint8Array([0x11,0x90])};
  for(let i=0;i<60;i++)await writer.add(new EncodedPacket(Uint8Array.of(i,3,4,5),'key',i*1024/48000,1024/48000),{decoderConfig:config});
  await output.finalize();const bytes=new Uint8Array(output.target.buffer);
  globalThis.MediaSource=class{static isTypeSupported(type){return type.startsWith('audio/mp4')}};
  globalThis.fetch=async(_url,{method,headers})=>{
    if(method==='HEAD')return new Response(null,{headers:{'Content-Length':String(bytes.length)}});
    const [,a,b]=/^bytes=(\d+)-(\d+)$/.exec(headers.Range);
    return new Response(bytes.slice(+a,+b+1),{status:206,headers:{'Content-Range':`bytes ${a}-${b}/${bytes.length}`}});
  };
  const source=new MediaRangeSource('https://test/m4a'),signal=new AbortController().signal;
  let decoded;
  try{
    await source.open(signal);const info=await inspectMedia(source,signal);
    assert.equal(info.tracks.length,1);assert.equal(info.tracks[0].transcode,false);
    // 模拟窗口处于延迟起始轨道之前，不得回退为读取整条轨道。
    assert.equal(await remuxMediaSlice(source,info.tracks[0],-1,-0.5,info.origin,signal),null);
    const slice=await remuxMediaSlice(source,info.tracks[0],0.7,1,info.origin,signal);
    assert.match(slice.mime,/^audio\/mp4/);assert.equal(slice.eof,false);
    decoded=new Input({source:new BlobSource(new Blob([slice.data])),formats:ALL_FORMATS});
    const track=await decoded.getPrimaryAudioTrack(),packet=await new EncodedPacketSink(track).getFirstPacket();
    assert.ok(packet.timestamp>0.65&&packet.timestamp<0.71);
    assert.deepEqual([...packet.data],[Math.floor(0.7*48000/1024),3,4,5]);
    assert.ok(slice.end>=1&&slice.end<1.05);
  }finally{
    decoded?.dispose();source.clear();globalThis.fetch=originalFetch;
    if(originalMse===undefined)delete globalThis.MediaSource;else globalThis.MediaSource=originalMse;
  }
});
