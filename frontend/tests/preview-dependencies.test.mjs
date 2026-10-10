import assert from 'node:assert/strict';
import test from 'node:test';
import ExcelJS from 'exceljs';
import MarkdownIt from 'markdown-it';
import katexPlugin from '@traptitech/markdown-it-katex';
import * as echarts from 'echarts';
import mammoth from 'mammoth';
import JSZip from 'jszip';

test('升级预览依赖后 Excel 条件格式与工作簿往返兼容', async () => {
  const workbook = new ExcelJS.Workbook(), sheet = workbook.addWorksheet('数据');
  sheet.getCell('A1').value = 42;
  sheet.addConditionalFormatting({ ref: 'A1', rules: [{ type: 'iconSet', iconSet: '3Triangles', cfvo: [{ type: 'percent', value: 0 }, { type: 'percent', value: 33 }, { type: 'percent', value: 67 }] }] });
  const buffer = await workbook.xlsx.writeBuffer();
  const parsed = new ExcelJS.Workbook();
  await parsed.xlsx.load(buffer);
  assert.equal(parsed.getWorksheet('数据').getCell('A1').value, 42);
});

test('升级 KaTeX 后 Markdown 公式与文本正常渲染', () => {
  const html = new MarkdownIt().use(katexPlugin).render('示例 $x^2 + y^2$');
  assert.match(html, /示例/);
  assert.match(html, /class="katex"/);
  assert.doesNotMatch(html, /katex-error/);
});

test('升级 ECharts 后 PPTX 使用的图表 API 仍可生成 SVG', () => {
  const chart = echarts.init(null, null, { renderer: 'svg', ssr: true, width: 400, height: 240 });
  try {
    chart.setOption({ animation: false, xAxis: { data: ['一', '二'] }, yAxis: {}, series: [{ type: 'bar', data: [10, 20] }] });
    const svg = chart.renderToSVGString();
    assert.match(svg, /<svg/);
    assert.match(svg, /<path/);
  } finally { chart.dispose(); }
});

test('Mammoth 的前端转换不依赖旧版 CLI 参数解析器', async () => {
  const zip = new JSZip();
  zip.file('[Content_Types].xml', '<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>');
  zip.file('_rels/.rels', '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>');
  zip.file('word/document.xml', '<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>预览测试</w:t></w:r></w:p></w:body></w:document>');
  const result = await mammoth.convertToHtml({ buffer: await zip.generateAsync({ type: 'nodebuffer' }) });
  assert.match(result.value, /<p>预览测试<\/p>/);
});

test('替换后的 PDF 引擎实际加载文档并读取页面', async () => {
  const { getDocument } = await import('pdfjs-dist/legacy/build/pdf.mjs');
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 300] >>',
  ];
  let content = '%PDF-1.4\n';
  const offsets = [0];
  objects.forEach((object, index) => {
    offsets.push(Buffer.byteLength(content));
    content += `${index + 1} 0 obj\n${object}\nendobj\n`;
  });
  const xref = Buffer.byteLength(content);
  content += `xref\n0 4\n0000000000 65535 f \n${offsets.slice(1).map(offset => String(offset).padStart(10, '0') + ' 00000 n \n').join('')}trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF`;
  const task = getDocument({ data: new Uint8Array(Buffer.from(content)) });
  try {
    const document = await task.promise;
    assert.equal(document.numPages, 1);
    const page = await document.getPage(1);
    const viewport = page.getViewport({ scale: 1 });
    assert.equal(viewport.width, 200);
    assert.equal(viewport.height, 300);
  } finally { await task.destroy(); }
});
