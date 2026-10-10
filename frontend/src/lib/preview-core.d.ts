// 锁定版本发布包的解析辅助函数；实际模块由 Vite 指向 npm 产物。
declare module "@preview-core" {
  export const b: string[];
  export function i(bytes: ArrayBuffer): Promise<Record<string, unknown>>;
  export function X(bytes: ArrayBuffer): Uint8Array;
  export function Y(bytes: Uint8Array): boolean;
  export function G(bytes: Uint8Array): { sheets: unknown[] };
  export function x(workbook: unknown): unknown;
  export function D(workbook: unknown): unknown;
}

declare module "virtual:xph-preview-markdown" {
  export function renderMarkdown(content: string): string;
}
