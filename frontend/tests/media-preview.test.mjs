import assert from 'node:assert/strict';
import test from 'node:test';
import {build} from 'esbuild';
import {resolve} from 'node:path';
import {Output,BufferTarget,Mp4OutputFormat,WebMOutputFormat,EncodedAudioPacketSource,EncodedVideoPacketSource,EncodedPacket,Input,BlobSource,ALL_FORMATS,EncodedPacketSink} from 'mediabunny';
async function module(path) {
  const result=await build({entryPoints:[resolve(path)],bundle:true,write:false,format:'esm',platform:'node',external:['mediabunny'],logLevel:'silent'});
  return import(`data:text/javascript;base64,${Buffer.from(result.outputFiles[0].text.replaceAll('"mediabunny"',JSON.stringify(import.meta.resolve('mediabunny')))).toString('base64')}`);
}
const {MediaRangeSource,FULL_PREVIEW_LIMIT}=await module('src/lib/mediaRangeSource.ts');
const {MediaDemuxSession,setMediaSupportCheck}=await module('src/lib/mediaRemux.ts');
const {mediaKind,mediaMimeType}=await module('src/lib/mediaFormats.ts');
const {mediaSourceConstructor,supportsMediaSourceType}=await module('src/lib/mediaSourceSupport.ts');
async function encrypt(plain,log2=9) {
  const dek=crypto.getRandomValues(new Uint8Array(32)),key=await crypto.subtle.importKey('raw',dek,'AES-GCM',false,['encrypt']);
  const header=new Uint8Array(64);header.set(new TextEncoder().encode('XPHCRPT1'));header[8]=1;header[9]=log2;
  new DataView(header.buffer).setBigUint64(28,BigInt(plain.length),true);new DataView(header.buffer).setUint32(36,12345,true);
  const blocks=[header],size=2**log2;
  for(let i=0;i<Math.ceil(plain.length/size);i++){
    const iv=new Uint8Array(12);new DataView(iv.buffer).setUint32(0,12345);new DataView(iv.buffer).setBigUint64(4,BigInt(i));
    blocks.push(new Uint8Array(await crypto.subtle.encrypt({name:'AES-GCM',iv,additionalData:header.slice(0,40)},key,plain.slice(i*size,(i+1)*size))));
  }
  return {cipher:new Uint8Array(await new Blob(blocks).arrayBuffer()),dek};
}
async function fixture(plain,options={}) {
  const {cipher,dek}=await encrypt(plain,options.log2||9);
  const cut=64+3*((2**(options.log2||9))+16);
  const volumes=options.multi&&cut<cipher.length?[cipher.slice(0,cut),cipher.slice(cut)]:[cipher];
  let offset=0;const parts=volumes.map((bytes,i)=>{const p={url:`https://test/${i}`,offset,size:bytes.length};offset+=bytes.length;return p});
  const reads=[];let active=0,max=0;
  const original=globalThis.fetch;
  globalThis.fetch=async(url,{headers,signal})=>{
    signal.throwIfAborted();const [,a,b]=/^bytes=(\d+)-(\d+)$/.exec(headers.Range);reads.push([url,+a,+b]);
    active++;max=Math.max(max,active);await new Promise(r=>setTimeout(r,1));active--;signal.throwIfAborted();
    const bytes=volumes[parts.findIndex(p=>p.url===url)];
    return new Response(bytes.slice(+a,+b+1),{status:206,headers:{'Content-Range':`bytes ${a}-${b}/${bytes.length}`}});
  };
  const abort=new AbortController(),descriptor={url:parts[0].url,parts,key:dek.slice(),plainSize:plain.length,cipherSize:cipher.length};
  const source=new MediaRangeSource(descriptor);await source.open(abort.signal);
  return {source,reads,cipher,dek,parts,descriptor,abort,max:()=>max,close(){abort.abort();globalThis.fetch=original}};
}
async function mediaFile({video=false,webm=false,count=600}={}) {
  const output=new Output({target:new BufferTarget(),format:webm?new WebMOutputFormat():new Mp4OutputFormat({fastStart:false})});
  const writer=video?new EncodedVideoPacketSource('avc'):new EncodedAudioPacketSource(webm?'opus':'aac');
  if(video)output.addVideoTrack(writer);else output.addAudioTrack(writer);await output.start();
  const config=video?{codec:'avc1.42001f',codedWidth:64,codedHeight:64,description:new Uint8Array([1,0x42,0,0x1f,0xff,0xe0,0])}:webm?{codec:'opus',sampleRate:48000,numberOfChannels:1}:{codec:'mp4a.40.2',sampleRate:48000,numberOfChannels:2,description:new Uint8Array([0x11,0x90])};
  for(let i=0;i<count;i++)await writer.add(new EncodedPacket(video?Uint8Array.of(0,0,0,1,i%255):webm?Uint8Array.of(0xf8,0xff,0xfe):Uint8Array.of(i%256,3,4,5),video&&i%240!==0?'delta':'key',i*(video?1/24:0.02),video?1/24:0.02),{decoderConfig:config});
  await output.finalize();return new Uint8Array(output.target.buffer);
}

test('媒体格式分类与 ManagedMediaSource 能力检查',()=>{
  for(const ext of ['mp4','m4v','mov','webm','mkv','3gp','mts','m2ts'])assert.equal(mediaKind('a.'+ext),'video');
  for(const ext of ['mp3','wav','m4a','flac','aac','ogg','opus'])assert.equal(mediaKind('a.'+ext),'audio');
  assert.equal(mediaKind('a.ts'),null);assert.equal(mediaKind('a.ts','video/mp2t'),'video');assert.equal(mediaMimeType('a.opus','application/octet-stream'),'audio/ogg');
  globalThis.ManagedMediaSource=class{static isTypeSupported(mime){return mime==='audio/mp4'}};
  try{assert.equal(mediaSourceConstructor(),globalThis.ManagedMediaSource);assert.equal(supportsMediaSourceType('audio/mp4'),true)}finally{delete globalThis.ManagedMediaSource}
});
test('真实 AES 跨卷、跨块、尾部随机读取和完整兜底共用缓存',async()=>{
  const plain=Uint8Array.from({length:512*20+71},(_,i)=>i%251),f=await fixture(plain,{multi:true});
  try{
    const [a,b]=await Promise.all([f.source.read(500,3100,f.abort.signal),f.source.read(520,1060,f.abort.signal)]);
    assert.deepEqual(a,plain.slice(500,3100));assert.deepEqual(b,plain.slice(520,1060));
    assert.deepEqual(await f.source.read(plain.length-30,plain.length,f.abort.signal),plain.slice(-30));
    const progress=[];assert.deepEqual(new Uint8Array(await (await f.source.complete('video/mp4',n=>progress.push(n))).arrayBuffer()),plain);
    const reads=f.reads.length;await f.source.complete('video/mp4',()=>{});assert.equal(f.reads.length,reads);assert.equal(progress.at(-1),plain.length);assert.ok(f.max()<=4);
  }finally{f.close()}
});
test('完整 GCM 认证前不泄露块，损坏文件不会进入完整兜底成功路径',async()=>{
  const plain=new Uint8Array(4096),f=await fixture(plain);
  try{f.cipher[64+4]^=1;globalThis.fetch=async(_url,{headers})=>{const[,a,b]=/^bytes=(\d+)-(\d+)$/.exec(headers.Range);return new Response(f.cipher.slice(+a,+b+1),{status:206})};
    await assert.rejects(f.source.read(0,10,f.abort.signal),{name:'XphFormatError'});
    await assert.rejects(f.source.complete('audio/mp4',()=>{}),{name:'XphFormatError'});
  }finally{f.close()}
});
test('有限元数据探测按认证块预算计数，不预取整批',async()=>{
  const f=await fixture(new Uint8Array(10000));let input;
  try{input=f.source.input(f.abort.signal,100);await assert.rejects(input.getFormat(),/读取上限/);assert.equal(f.reads.length,1)}finally{input?.dispose();f.close()}
});
test('连续 MP4 只生成一套初始化段，背压逐片等待；seek 复用 Input 和索引',async()=>{
  const f=await fixture(await mediaFile(),{multi:true});setMediaSupportCheck(async mime=>mime.startsWith('audio/mp4'));
  let opens=0;const input=f.source.input.bind(f.source);f.source.input=(...args)=>{opens++;return input(...args)};
  let decoded;
  try{
    const session=new MediaDemuxSession(f.source,f.abort.signal),info=await session.inspect(),chunks=[];let active=0,max=0;
    const receive=async(bytes,mime)=>{assert.match(mime,/^audio\/mp4/);active++;max=Math.max(max,active);await new Promise(r=>setTimeout(r,0));chunks.push(bytes);active--};
    const end=await session.stream(info.tracks[0],0,info.origin,f.abort.signal,receive,async()=>{});
    assert.ok(end>11.9);assert.ok(chunks.length>10);assert.equal(max,1);
    assert.equal(chunks.filter(b=>String.fromCharCode(...b.slice(4,8))==='ftyp').length,1);
    decoded=new Input({source:new BlobSource(new Blob(chunks)),formats:ALL_FORMATS});const track=await decoded.getPrimaryAudioTrack();let count=0;
    for await(const p of new EncodedPacketSink(track).packets()){assert.equal(p.data[0],count%256);count++}assert.equal(count,600);
    const seekChunks=[];await session.stream(info.tracks[0],6,info.origin,f.abort.signal,async b=>seekChunks.push(b),async()=>{});assert.equal(opens,1);
    const seekInput=new Input({source:new BlobSource(new Blob(seekChunks)),formats:ALL_FORMATS});try{const first=await new EncodedPacketSink(await seekInput.getPrimaryAudioTrack()).getFirstPacket();assert.ok(first.timestamp>=5.98&&first.timestamp<=6)}finally{seekInput.dispose()}
  }finally{decoded?.dispose();f.close()}
});
test('长 GOP 顺序解复用不重复包，背压在完整片段交付后执行',async()=>{
  const f=await fixture(await mediaFile({video:true}));setMediaSupportCheck(async mime=>mime.startsWith('video/mp4'));let decoded;
  try{const session=new MediaDemuxSession(f.source,f.abort.signal),info=await session.inspect(),chunks=[];let received=0;
    await session.stream(info.tracks[0],0,info.origin,f.abort.signal,async b=>{chunks.push(b);received++},async()=>assert.ok(received>0));
    decoded=new Input({source:new BlobSource(new Blob(chunks)),formats:ALL_FORMATS});const track=await decoded.getPrimaryVideoTrack();let count=0;
    for await(const p of new EncodedPacketSink(track).packets()){assert.equal(p.data.at(-1),count%255);count++}assert.equal(count,600);
  }finally{decoded?.dispose();f.close()}
});
test('取消旧输出不会销毁会话索引，下一次 seek 可继续',async()=>{
  const f=await fixture(await mediaFile());setMediaSupportCheck(async()=>true);
  try{const session=new MediaDemuxSession(f.source,f.abort.signal),info=await session.inspect(),seek=new AbortController();
    await assert.rejects(session.stream(info.tracks[0],0,info.origin,seek.signal,async()=>{seek.abort()},async()=>{}),{name:'AbortError'});
    let chunks=0;await session.stream(info.tracks[0],5,info.origin,f.abort.signal,async()=>{chunks++},async()=>{});assert.ok(chunks>1);
  }finally{f.close()}
});
test('WebM Cluster 连续交付并保留时间戳',async()=>{
  const f=await fixture(await mediaFile({webm:true,count:150}));setMediaSupportCheck(async mime=>mime.startsWith('audio/webm'));let decoded;
  try{const session=new MediaDemuxSession(f.source,f.abort.signal),info=await session.inspect(),chunks=[];
    await session.stream(info.tracks[0],0,info.origin,f.abort.signal,async b=>chunks.push(b),async()=>{});
    decoded=new Input({source:new BlobSource(new Blob(chunks)),formats:ALL_FORMATS});const sink=new EncodedPacketSink(await decoded.getPrimaryAudioTrack());assert.ok((await sink.getPacket(Infinity)).timestamp>2.9);assert.ok(chunks.length>=4);
  }finally{decoded?.dispose();f.close()}
});
test('上游忽略 Range 可复用原计划显式完整解密，不能无限创建 Blob',async()=>{
  const original=globalThis.fetch,plain=new Uint8Array(2000),{cipher,dek}=await encrypt(plain);let requests=0;
  globalThis.fetch=async()=>{requests++;return new Response(cipher)};
  const abort=new AbortController(),source=new MediaRangeSource({url:'https://test/full',key:dek,plainSize:plain.length,cipherSize:cipher.length});
  try{await assert.rejects(source.open(abort.signal),e=>e.code==='range');assert.equal(requests,1);
    assert.deepEqual(new Uint8Array(await (await source.complete('audio/mp4',()=>{})).arrayBuffer()),plain);assert.equal(requests,2);
    const large=new MediaRangeSource({url:'https://test/full',key:dek,plainSize:FULL_PREVIEW_LIMIT+1,cipherSize:FULL_PREVIEW_LIMIT+100});
    await assert.rejects(large.complete('',()=>{}),e=>e.code==='size');assert.equal(requests,2);
  }finally{abort.abort();globalThis.fetch=original}
});
test('临时网络错误有限重试，票据失效直接报错，关闭终止正在请求的块',async()=>{
  const f=await fixture(new Uint8Array(10000));const working=globalThis.fetch;let attempts=0;
  try{
    globalThis.fetch=async(...args)=>{if(++attempts===1)throw new TypeError('network');return working(...args)};
    await f.source.read(0,10,f.abort.signal);assert.equal(attempts,2);
    globalThis.fetch=async()=>new Response(null,{status:403});await assert.rejects(f.source.read(9000,9020,f.abort.signal),e=>e.code==='auth');
    let began;const started=new Promise(r=>began=r);globalThis.fetch=async(_url,{signal})=>new Promise((_,reject)=>{signal.addEventListener('abort',()=>reject(signal.reason),{once:true});began()});
    const read=f.source.read(8000,8020,f.abort.signal);await started;f.abort.abort();await assert.rejects(read,{name:'AbortError'});
  }finally{f.close()}
});

test('一个 Range 批次未下载完即可交付已认证的首块',async()=>{
  const f=await fixture(new Uint8Array(12000));let finish;
  globalThis.fetch=async(_url,{headers})=>{
    const [,a,b]=/^bytes=(\d+)-(\d+)$/.exec(headers.Range);
    return new Response(new ReadableStream({start(c){c.enqueue(f.cipher.slice(+a,+a+528));finish=()=>{c.enqueue(f.cipher.slice(+a+528,+b+1));c.close()}}}),{status:206});
  };
  try{assert.equal((await f.source.read(0,10,f.abort.signal)).length,10);assert.equal(typeof finish,'function');finish()}
  finally{f.close()}
});
test('多个随机区间共享并发上限，队列取消不遗留请求',async()=>{
  const plain=new Uint8Array(512*200),f=await fixture(plain);
  try{await Promise.all([0,9000,18000,27000,36000,45000,54000,63000,72000].map(start=>f.source.read(start,start+20,f.abort.signal)));assert.equal(f.max(),8)}finally{f.close()}
});
test('最后一块在响应长度核对后交付，额外密文字节不可静默忽略',async()=>{
  const f=await fixture(new Uint8Array(100));
  globalThis.fetch=async()=>new Response(new ReadableStream({start(c){c.enqueue(f.cipher.slice(64));c.enqueue(Uint8Array.of(1));c.close()}}),{status:206});
  try{await assert.rejects(f.source.read(0,10,f.abort.signal),e=>e.code==='range')}finally{f.close()}
});

test('播放器排空旧追加再 seek；连续跳转只启动最新代际，结束关闭释放资源',async()=>{
  const {attachMediaPlayback}=await module('src/lib/mediaPlayback.ts');
  const originalCreate=URL.createObjectURL,originalRevoke=URL.revokeObjectURL;let mse,releases=0;
  const ranges=(a,b)=>({length:b>a?1:0,start:()=>a,end:()=>b});
  class Buffer extends EventTarget{updating=false;buffered=ranges(0,0);mode='';changeType(){};appendBuffer(){this.updating=true;setTimeout(()=>{this.buffered=ranges(0,4);this.updating=false;this.dispatchEvent(new Event('updateend'))},5)}remove(){assert.equal(this.updating,false);this.updating=true;setTimeout(()=>{this.buffered=ranges(0,0);this.updating=false;this.dispatchEvent(new Event('updateend'))},1)}}
  globalThis.MediaSource=class extends EventTarget{readyState='closed';duration=0;constructor(){super();mse=this}static isTypeSupported(){return true}addSourceBuffer(){return new Buffer()}endOfStream(){this.readyState='ended'}};
  class Media extends EventTarget{currentTime=0;playbackRate=1;disableRemotePlayback=false;buffered=ranges(0,0);pause(){}removeAttribute(){this.src=''}load(){if(this.src)queueMicrotask(()=>{mse.readyState='open';mse.dispatchEvent(new Event('sourceopen'))})}}
  URL.createObjectURL=()=> 'blob:media';URL.revokeObjectURL=()=>releases++;
  const media=new Media(),root=new AbortController(),starts=[],failures=[];let receiver,token=0;
  const client={inspect:async()=>({tracks:[{mime:'audio/mp4'}],duration:100}),stop:()=>++token,start:(time)=>starts.push(time),demand(){},listen(chunk){receiver=chunk}};
  try{
    await attachMediaPlayback(media,client,root.signal,e=>failures.push(e));
    const old=token,pending=receiver(0,new Uint8Array(10),'audio/mp4',old);await Promise.resolve();
    media.currentTime=40;media.dispatchEvent(new Event('seeking'));media.currentTime=60;media.dispatchEvent(new Event('seeking'));
    const stale=receiver(0,new Uint8Array(10),'audio/mp4',old);const rejected=assert.rejects(stale,{name:'AbortError'});
    await pending;await rejected;await new Promise(r=>setTimeout(r,20));assert.deepEqual(starts,[0,60]);assert.equal(failures.length,0);
    root.abort();assert.equal(releases,1);assert.equal(media.src,'');
  }finally{root.abort();delete globalThis.MediaSource;URL.createObjectURL=originalCreate;URL.revokeObjectURL=originalRevoke}
});

test('大文件完整缓存只写密文，本地随机解密不再请求网络，关闭删除临时文件',async()=>{
  const plain=Uint8Array.from({length:12000},(_,i)=>i%239),f=await fixture(plain,{multi:true});
  const previous=Object.getOwnPropertyDescriptor(globalThis,'navigator');let saved,removed,created;
  const directory={async getFileHandle(name){created=name;return {async createWritable(){const chunks=[];return {async write(bytes){chunks.push(bytes.slice())},async close(){saved=new Blob(chunks)},async abort(){saved=null}}},async getFile(){return saved}}},async removeEntry(name){removed=name;saved=null}};
  Object.defineProperty(globalThis,'navigator',{configurable:true,value:{storage:{getDirectory:async()=>directory}}});
  try{
    await f.source.cacheCompleteCipher(()=>{});
    assert.deepEqual(new Uint8Array(await saved.arrayBuffer()),f.cipher);
    globalThis.fetch=async()=>assert.fail('完整密文缓存后不应再读取远程文件');
    assert.deepEqual(await f.source.read(9000,10000,f.abort.signal),plain.slice(9000,10000));
    assert.deepEqual(await f.source.read(0,1000,f.abort.signal),plain.slice(0,1000));
    f.abort.abort();await f.source.dispose();assert.equal(removed,created);assert.equal(saved,null);
  }finally{f.close();if(previous)Object.defineProperty(globalThis,'navigator',previous);else delete globalThis.navigator}
});
test('密文临时写入失败可删除残留文件，失败不回退无限内存缓冲',async()=>{
  const f=await fixture(new Uint8Array(10000));const previous=Object.getOwnPropertyDescriptor(globalThis,'navigator');let aborted=false,removed=false;
  Object.defineProperty(globalThis,'navigator',{configurable:true,value:{storage:{getDirectory:async()=>({
    getFileHandle:async()=>({createWritable:async()=>({write:async()=>{throw new DOMException('quota','QuotaExceededError')},abort:async()=>{aborted=true}})}),removeEntry:async()=>{removed=true},
  })}}});
  try{await assert.rejects(f.source.cacheCompleteCipher(()=>{}),{name:'QuotaExceededError'});await f.source.dispose();assert.equal(aborted,true);assert.equal(removed,true)}
  finally{f.close();if(previous)Object.defineProperty(globalThis,'navigator',previous);else delete globalThis.navigator}
});

test('长 GOP 缓冲回收不会跨过当前依赖的关键帧',async()=>{
  const {attachMediaPlayback}=await module('src/lib/mediaPlayback.ts');
  const originalCreate=URL.createObjectURL,originalRevoke=URL.revokeObjectURL;let mse;const removed=[];
  const range={length:1,start:()=>0,end:()=>200};
  class Buffer extends EventTarget{updating=false;buffered=range;mode='';appendBuffer(){this.updating=true;queueMicrotask(()=>{this.updating=false;this.dispatchEvent(new Event('updateend'))})}remove(from,to){removed.push([from,to]);queueMicrotask(()=>this.dispatchEvent(new Event('updateend')))}}
  globalThis.MediaSource=class extends EventTarget{readyState='closed';duration=0;constructor(){super();mse=this}static isTypeSupported(){return true}addSourceBuffer(){return new Buffer()}endOfStream(){}};
  class Media extends EventTarget{currentTime=0;playbackRate=1;buffered=range;pause(){}removeAttribute(){this.src=''}load(){if(this.src)queueMicrotask(()=>{mse.readyState='open';mse.dispatchEvent(new Event('sourceopen'))})}}
  URL.createObjectURL=()=> 'blob:media';URL.revokeObjectURL=()=>{};
  const media=new Media(),root=new AbortController();let receiver,token=0;
  const client={inspect:async()=>({tracks:[{type:'video',mime:'video/mp4'}],duration:200}),stop:()=>++token,start(){},demand(){},listen(chunk){receiver=chunk}};
  try{
    await attachMediaPlayback(media,client,root.signal,assert.fail);
    await receiver(0,new Uint8Array(10),'video/mp4',token,[0,100]);
    media.currentTime=40;media.dispatchEvent(new Event('timeupdate'));await new Promise(r=>setTimeout(r,0));assert.equal(removed.length,0);
    media.currentTime=130;media.dispatchEvent(new Event('timeupdate'));await new Promise(r=>setTimeout(r,0));assert.equal(removed.length,1);assert.ok(removed[0][1]<100&&removed[0][1]>99.9);
  }finally{root.abort();delete globalThis.MediaSource;URL.createObjectURL=originalCreate;URL.revokeObjectURL=originalRevoke}
});
