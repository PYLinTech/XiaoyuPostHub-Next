import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import ts from 'typescript';
const code = ts.transpileModule(await readFile(new URL('../src/lib/pdfSession.ts', import.meta.url), 'utf8'), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 },
}).outputText;
const { createPdfSession } = await import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`);
function task() {
  let resolve, reject, stops = 0;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject, destroy: async () => { stops++; }, get stops() { return stops; } };
}
test('关闭预览取消 PDF loadingTask，迟到文档不会激活', async () => {
  const pending = task(), session = createPdfSession(() => pending);
  const result = session.load({ url: 'first' });
  session.dispose();
  assert.equal(pending.stops, 1);
  pending.resolve({});
  assert.equal(await result, null);
  assert.equal(pending.stops, 1);
  assert.equal(await session.load({ url: 'later' }), null);
});
test('切换 PDF 取消旧请求，旧请求错误不覆盖新文档', async () => {
  const first = task(), second = task();
  let requests = 0;
  const session = createPdfSession(() => requests++ ? second : first);
  const old = session.load({ url: 'first' }), current = session.load({ url: 'second' });
  assert.equal(first.stops, 1);
  first.reject(new Error('旧请求被取消'));
  const document = { destroy: async () => {} };
  second.resolve(document);
  assert.equal(await old, null);
  assert.equal(await current, document);
  session.dispose();
  assert.equal(second.stops, 1);
  session.releaseDocument();
  assert.equal(second.stops, 1);
});
test('当前 PDF 加载错误正常向渲染器报告', async () => {
  const pending = task(), session = createPdfSession(() => pending);
  const result = session.load({ url: 'bad' });
  pending.reject(new Error('PDF 文件损坏'));
  await assert.rejects(result, /PDF 文件损坏/);
  session.dispose();
});
