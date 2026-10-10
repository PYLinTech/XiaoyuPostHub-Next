import assert from 'node:assert/strict';
import test from 'node:test';
import { Worker } from 'node:worker_threads';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { build } from 'esbuild';
import ExcelJS from 'exceljs';
import JSZip from 'jszip';
import {previewMarkdownModule} from '../build/previewCompatibility.mjs';

const root = resolve('.');
const temporary = await mkdtemp(join(tmpdir(), 'xph-preview-'));
const bundle = join(temporary, 'parser.mjs');
await build({ entryPoints: [join(root, 'src/lib/previewParser.worker.ts')], outdir: temporary, entryNames: "parser", outExtension: { ".js": ".mjs" }, splitting: true,
  bundle: true, format: 'esm', platform: 'browser', logLevel: 'silent', external: ['three/*', 'util', 'zlib'],
  plugins: [{name:'published-markdown-parser',setup(build) {
    build.onResolve({filter:/^virtual:xph-preview-markdown$/},()=>({path:'markdown',namespace:'preview'}));
    build.onLoad({filter:/.*/,namespace:'preview'},async()=>({contents:previewMarkdownModule(await readFile(join(root,'node_modules/@eternalheart/vue-file-preview/lib/chunks/index-B6pWnG9T.mjs'),'utf8')),resolveDir:root}));
  }}],
  alias: { '@': join(root, 'src'), '@preview-core': join(root, 'node_modules/@eternalheart/vue-file-preview/lib/chunks/index-CyaXSkiB.mjs') } });
const bootstrap = join(temporary, 'bootstrap.mjs');
await writeFile(bootstrap, `import {parentPort} from 'node:worker_threads';
  globalThis.self=globalThis;
  globalThis.postMessage=value=>parentPort.postMessage(value);
  await import(${JSON.stringify(pathToFileURL(bundle).href)});
  parentPort.on('message',data=>{
    if(data.kind==='decrypt') globalThis.fetch=async(_url,{headers})=>{
      const [,start,end]=/^bytes=(\\d+)-(\\d+)$/.exec(headers.Range);
      return new Response(data.value.cipher.slice(Number(start),Number(end)+1),{status:206});
    };
    globalThis.onmessage({data});
  });
  parentPort.postMessage({ready:true});`);
test.after(() => rm(temporary, { recursive: true, force: true }));
function parse(kind, value) {
  const worker = new Worker(bootstrap);
  return new Promise((resolve, reject) => {
    worker.on('error', reject);
    worker.on('message', data => {
      if (data.ready) worker.postMessage({ id: 1, kind, value });
      else if (data.error) reject(new Error(data.error.message));
      else resolve(data.value);
    });
  }).finally(() => worker.terminate());
}
test('真正独立线程解析 Excel 并返回发布库使用的表格数据', async () => {
  const workbook = new ExcelJS.Workbook();
  workbook.addWorksheet('测试').getCell('A1').value = '线程数据';
  const bytes = await workbook.xlsx.writeBuffer();
  const result = await parse('xlsx', bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength));
  assert.match(JSON.stringify(result), /线程数据/);
  assert.match(JSON.stringify(result), /测试/);
});
test('真正独立线程转换 DOCX 内容与样式', async () => {
  const zip = new JSZip();
  zip.file('[Content_Types].xml', '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>');
  zip.file('_rels/.rels', '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>');
  zip.file('word/document.xml', '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>独立预览</w:t></w:r></w:p></w:body></w:document>');
  const [html, metadata] = await parse('docx', await zip.generateAsync({type:'arraybuffer'}));
  assert.match(html.value, /独立预览/);
  assert.equal(typeof metadata, 'object');
});
test('语法高亮在线程中执行且保留行号', async () => {
  const result = await parse('highlight', {code:'const a = 1;',options:{lang:'javascript',theme:'github-light'},lineNumbers:true});
  assert.match(result, /data-line="1"/);
  assert.match(result, /class="line"/);
});

test('Markdown 使用发布库原有配置在线程解析，保留公式及代码块样式', async () => {
  const html=await parse('markdown', '# 标题\n\n$x^2$\n\n```js\nconst a=1;\n```');
  assert.match(html, /<h1>标题<\/h1>/);
  assert.match(html, /class="katex"/);
  assert.match(html, /code-block-wrapper/);
  assert.match(html, /data-shiki-pending="1"/);
});


test('完整预览在线程解密后校验摘要，拒绝与记录不一致的文件', async () => {
  const plain = new TextEncoder().encode('预览完整性校验');
  const dek = crypto.getRandomValues(new Uint8Array(32));
  const key = await crypto.subtle.importKey('raw', dek, 'AES-GCM', false, ['encrypt']);
  const header = new Uint8Array(64);
  header.set(new TextEncoder().encode('XPHCRPT1')); header[8] = 1; header[9] = 9;
  new DataView(header.buffer).setBigUint64(28, BigInt(plain.length), true);
  const body = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: new Uint8Array(12), additionalData: header.slice(0,40) }, key, plain));
  const cipher = new Uint8Array(64 + body.length); cipher.set(header); cipher.set(body,64);
  const checksum = Buffer.from(await crypto.subtle.digest('SHA-256',plain)).toString('hex');
  const plan = {mode:'direct',url:'https://storage.test/cipher',mimeType:'text/plain',checksum};
  const blob = await parse('decrypt',{plan,dek,token:'',cipher});
  assert.equal(await blob.text(), new TextDecoder().decode(plain));
  await assert.rejects(parse('decrypt',{plan:{...plan,checksum:'invalid'},dek,token:'',cipher}), /完整性校验失败/);
});
