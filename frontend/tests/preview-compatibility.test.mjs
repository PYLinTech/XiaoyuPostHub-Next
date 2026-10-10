import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import test from 'node:test';
import ts from 'typescript';
import { adaptPreviewBundle } from '../build/previewCompatibility.mjs';

const rendererPath = resolve('node_modules/@eternalheart/vue-file-preview/lib/chunks/index-COB3-Z9z.mjs');
const mainPath = resolve('node_modules/@eternalheart/vue-file-preview/lib/chunks/index-CyaXSkiB.mjs');

test('PDF 使用修复版 npm 引擎与本地 Worker，适配产物没有缺失变量', async () => {
  const code = adaptPreviewBundle(await readFile(rendererPath, 'utf8'), rendererPath + '?v=test');
  assert.match(code, /from "pdfjs-dist\/legacy\/build\/pdf.mjs"/);
  assert.doesNotMatch(code, /pdf.worker-6kySV6rJ|6\.1\.200/);
  const options = { allowJs: true, checkJs: true, noEmit: true, target: ts.ScriptTarget.ES2022 };
  const host = ts.createCompilerHost(options), getSourceFile = host.getSourceFile;
  host.getSourceFile = (name, ...args) => name === '/preview-check.js'
    ? ts.createSourceFile(name, code, options.target, true, ts.ScriptKind.JS)
    : getSourceFile.call(host, name, ...args);
  const diagnostics = ts.createProgram(['/preview-check.js'], options, host).getSemanticDiagnostics();
  assert.deepEqual(diagnostics.filter(d => d.code === 2304).map(d => ts.flattenDiagnosticMessageText(d.messageText, ' ')), []);
  const main = adaptPreviewBundle(await readFile(mainPath, 'utf8'), mainPath);
  assert.doesNotMatch(main, /unpkg\.com\/pdfjs-dist/);
  assert.match(main, /\/pdfjs\/pdf.worker.min.mjs/);
});

test('恶意 PDF 大纲标题只能作为文本，不能注入 HTML 或属性', async () => {
  const code = adaptPreviewBundle(await readFile(rendererPath, 'utf8'), rendererPath);
  const helper = code.slice(code.indexOf('function escapeOutline'), code.indexOf('\nconst ew'));
  const start = code.indexOf('N = (L, B = 0) =>') + 'N = '.length;
  const end = code.indexOf('}).join(""), $ =', start) + '}).join("")'.length;
  const render = new Function('v', `${helper}\nreturn ${code.slice(start, end)}`)({ value: null });
  const html = render([{ title: '"><img src=x onerror="alert(1)">', dest: [], items: [] }]);
  assert.doesNotMatch(html, /<img|title="">/);
  assert.match(html, /&lt;img/);
});

test('发布包文档 HTML 的正式输出和临时容器均清洗，保留正常排版', async () => {
  const { JSDOM } = await import('jsdom');
  const { default: createDOMPurify } = await import('dompurify');
  const window = new JSDOM('').window;
  try {
    const DOMPurify = createDOMPurify(window);
    const path = resolve('node_modules/@eternalheart/vue-file-preview/lib/chunks/index-B6pWnG9T.mjs');
    const code = adaptPreviewBundle(await readFile(path, 'utf8'), path);
    assert.match(code, /S\.innerHTML = DOMPurify\.sanitize\(g\)/);
    assert.match(code, /innerHTML: DOMPurify\.sanitize\(h\.value\)/);
    const source = ts.createSourceFile('renderer.mjs', code, ts.ScriptTarget.ES2022, true, ts.ScriptKind.JS);
    let expression;
    function visit(node) {
      if (ts.isPropertyAssignment(node) && node.name.getText(source) === 'innerHTML'
        && node.initializer.getText(source).includes('h.value')) expression = node.initializer.getText(source);
      ts.forEachChild(node, visit);
    }
    visit(source);
    const html = new Function('DOMPurify', 'h', `return ${expression}`)(DOMPurify, {
      value: '<h2>标题</h2><script>alert(1)</script><img src=x onerror="alert(1)"><a href="javascript:alert(1)">链接</a>',
    });
    const document = new JSDOM(html).window.document;
    assert.equal(document.querySelector('h2').textContent, '标题');
    assert.equal(document.querySelector('script'), null);
    assert.equal(document.querySelector('img').getAttribute('onerror'), null);
    assert.equal(document.querySelector('a').getAttribute('href'), null);
  } finally {
    window.close();
  }
});

test('PPTX 文本写入 HTML 前也经过清洗', async () => {
  const path = resolve('node_modules/pptx-preview/dist/pptx-preview.es.js');
  const code = adaptPreviewBundle(await readFile(path, 'utf8'), path);
  assert.match(code, /innerHTML=DOMPurify\.sanitize\("string"==typeof c\?c:""\)/);
});
