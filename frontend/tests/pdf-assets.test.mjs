import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { pdfAssetsPlugin } from '../build/pdfAssets.mjs';

test('开发期本地 PDF Worker 可读取，非法路径被拒绝', async () => {
  let middleware;
  pdfAssetsPlugin().configureServer({ middlewares: { use(fn) { middleware = fn; } } });
  async function request(url, method = 'GET') {
    const res = { headers: {}, statusCode: 200, setHeader(name, value) { this.headers[name] = value; }, end(body) { this.body = body; } };
    await middleware({ url, method }, res, () => { res.next = true; });
    return res;
  }
  const worker = await request('/pdfjs/pdf.worker.min.mjs');
  assert.equal(worker.statusCode, 200);
  assert.equal(worker.headers['Content-Type'], 'text/javascript');
  assert.ok(worker.body.length > 1000);
  const head = await request('/pdfjs/pdf.worker.min.mjs', 'HEAD');
  assert.equal(head.body, undefined);
  assert.equal(head.headers['Content-Length'], worker.body.length);
  for (const url of ['/pdfjs/wasm/%2e%2e/%2e%2e/package.json', '/pdfjs/wasm/..%5cpackage.json', '/pdfjs/%zz']) {
    assert.equal((await request(url)).statusCode, 400);
  }
  assert.equal((await request('/pdfjs/package.json')).statusCode, 404);
  assert.equal((await request('/api/health')).next, true);
});

test('生产资源复制路径与开发地址一致', async () => {
  const root = await mkdtemp(join(tmpdir(), 'xph-pdf-assets-'));
  try {
    const plugin = pdfAssetsPlugin();
    plugin.configResolved({ root, build: { outDir: 'dist' } });
    await plugin.closeBundle();
    const path = join(root, 'dist/pdfjs');
    assert.ok((await readFile(join(path, 'pdf.worker.min.mjs'))).length > 1000);
    for (const directory of ['cmaps', 'standard_fonts', 'wasm']) assert.equal((await stat(join(path, directory))).isDirectory(), true);
  } finally { await rm(root, { recursive: true, force: true }); }
});
