import { packError } from "@/delivery/transferProtocol";
import type { PreviewParseKind } from "./previewParser";

self.onmessage = async ({ data }: MessageEvent<{ id: number; kind: PreviewParseKind; value: any }>) => {
  try {
    let value: unknown;
    switch (data.kind) {
      case "decrypt": {
        const [{ setToken }, xph, delivery, { Sha256, bytesToHex }] = await Promise.all([
          import("@/api/client"), import("@/crypto/xph"), import("@/delivery/download"), import("@/crypto/sha256"),
        ]);
        const { plan, dek, token } = data.value;
        setToken(token);
        const key = await xph.importContentKey(dek);
        const bytes = await delivery.fetchCipherPlanRange(plan, 0, xph.HEADER_SIZE - 1);
        const header = xph.parseXphHeader(bytes);
        delivery.assertHeaderMatchesMeta(header, plan.encryption);
        const parts: Uint8Array[] = [];
        const hasher = new Sha256();
        await xph.decryptAll(key, header,
          (start, end) => delivery.fetchCipherPlanRange(plan, start, end - 1),
          plain => { hasher.update(plain); parts.push(plain); }, { concurrency: 6, batchBlocks: 8 });
        if (bytesToHex(hasher.digest()) !== plan.checksum) throw new Error("预览文件完整性校验失败");
        value = new Blob(parts as BlobPart[], { type: plan.mimeType || "application/octet-stream" });
        break;
      }
      case "docx": {
        const [{ default: mammoth }, core] = await Promise.all([import("mammoth"), import("@preview-core")]);
        const [html, metadata] = await Promise.all([
          mammoth.convertToHtml({ arrayBuffer: data.value }, { styleMap: core.b }),
          core.i(data.value).catch(() => ({})),
        ]);
        value = [html, metadata];
        break;
      }
      case "xlsx": {
        const [{ default: ExcelJS }, core] = await Promise.all([import("exceljs"), import("@preview-core")]);
        const bytes = core.X(data.value);
        if (!bytes.byteLength) throw new Error("文件为空");
        if (core.Y(bytes)) {
          const workbook = core.G(bytes);
          if (!workbook || !Array.isArray(workbook.sheets)) throw new Error("Excel 文件格式无效或已损坏");
          value = core.x(workbook);
        } else {
          const workbook = await new ExcelJS.Workbook().xlsx.load(bytes.slice().buffer);
          value = core.D(workbook);
        }
        break;
      }
      case "markdown": {
        const { renderMarkdown } = await import("virtual:xph-preview-markdown");
        value = renderMarkdown(data.value);
        break;
      }
      case "highlight": {
        const { codeToHtml } = await import("shiki");
        value = await codeToHtml(data.value.code, {
          ...data.value.options,
          transformers: data.value.lineNumbers ? [{ name: "line-numbers", line(node, line) {
            node.properties["data-line"] = line;
            this.addClassToHast(node, "line");
          } }] : undefined,
        });
        break;
      }
      default: throw new Error("未知预览解析任务");
    }
    self.postMessage({ id: data.id, value });
  } catch (error) {
    self.postMessage({ id: data.id, error: packError(error) });
  }
};
