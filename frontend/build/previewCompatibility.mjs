import ts from "typescript";

// 适配锁定版本的 npm 发布产物，不在仓库保存第三方源码。
// 上游将 PDF.js 打入渲染器，单独升级依赖不会替换内置引擎。
export function adaptPreviewBundle(code, id) {
  const path = id.split('?')[0];
  const wrapper = path.includes('/@eternalheart/vue-file-preview/lib/') && path.endsWith('.mjs');
  const pptx = path.endsWith('/pptx-preview/dist/pptx-preview.es.js');
  if (!wrapper && !pptx) return;
  if (code.includes('__name: "index"') && code.includes('data-outline-key=')) {
    const marker = ')), ew = { class:';
    const index = code.indexOf(marker);
    if (index < 0 || !code.includes('getDocument: fm') || !code.includes('GlobalWorkerOptions: wi')) {
      throw new Error('预览库 PDF 发布产物已变化，请重新验证兼容适配');
    }
    const imports = [...code.matchAll(/^import .*;$/gm)].map(match => match[0]).join('\n');
    let renderer = 'const ew = { class:' + code.slice(index + marker.length);
    // Electron 也使用本地 Worker，不加载上游内置的旧版本。
    const electron = /        if \(typeof navigator < "u"[\s\S]*?        const B = Mm\(\);/;
    if (!electron.test(renderer)) throw new Error('预览库 PDF Worker 初始化已变化');
    renderer = renderer.replace(electron, '        const B = Mm();');
    const load = /await s\(\), E = await fm\(\{([\s\S]*?)\}\)\.promise;/;
    if (!load.test(renderer)) throw new Error('预览库 PDF 加载方式已变化');
    renderer = renderer.replace('let E = null;', 'const pdfSession = createPdfSession(tw.getDocument); zb(() => pdfSession.dispose()); let E = null;')
      .replaceAll('E.destroy();', 'pdfSession.releaseDocument();')
      .replace(load, 'await s(); const loaded = await pdfSession.load({$1}); if (!loaded || pdfSession.disposed) return; E = loaded;');
    const outlineStart = renderer.indexOf('N = (L, B = 0) =>');
    const outlineEnd = renderer.indexOf('}).join(""), $ =', outlineStart);
    if (outlineStart < 0 || outlineEnd < 0) throw new Error('预览库 PDF 大纲渲染已变化');
    renderer = renderer.slice(0, outlineStart) + renderer.slice(outlineStart, outlineEnd).replaceAll('G.title', 'escapeOutline(G.title)') + renderer.slice(outlineEnd);
    code = `${imports}
import * as tw from "pdfjs-dist/legacy/build/pdf.mjs";
import {createPdfSession} from "@/lib/pdfSession";
const {GlobalWorkerOptions: wi} = tw;
function escapeOutline(value) {
  return String(value).replace(/[&<>"']/g, char => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
}
${renderer}`;
  }
  code = sanitizeRendererHTML(code);
  return code
    .replace(/https:\/\/unpkg\.com\/pdfjs-dist@\$\{[^}]+\}\/legacy\/build\/pdf\.worker\.min\.mjs/g, '/pdfjs/pdf.worker.min.mjs')
    .replace(/https:\/\/unpkg\.com\/pdfjs-dist@\$\{[^}]+\}\/(cmaps|standard_fonts|wasm)\//g, '/pdfjs/$1/');
}

// 文档与 Markdown 渲染器输出的 HTML 进入 DOM 前统一清洗。
// 遍历语法节点，避免字符串替换误伤属性表达式或漏掉临时容器的赋值。
function sanitizeRendererHTML(code) {
  if (!code.includes('innerHTML')) return code;
  const source = ts.createSourceFile('preview.mjs', code, ts.ScriptTarget.ES2022, true, ts.ScriptKind.JS);
  const edits = [];
  function visit(node) {
    let value;
    if (ts.isPropertyAssignment(node) && node.name.getText(source) === 'innerHTML') {
      value = node.initializer;
    } else if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken
      && ts.isPropertyAccessExpression(node.left) && node.left.name.text === 'innerHTML') {
      value = node.right;
    }
    if (value && !ts.isStringLiteral(value)) {
      edits.push({start: value.getStart(source), end: value.end});
    }
    ts.forEachChild(node, visit);
  }
  visit(source);
  if (!edits.length) return code;
  for (const {start, end} of edits.sort((a, b) => b.start - a.start)) {
    code = code.slice(0, start) + 'DOMPurify.sanitize(' + code.slice(start, end) + ')' + code.slice(end);
  }
  return 'import DOMPurify from "dompurify";\n' + code;
}
