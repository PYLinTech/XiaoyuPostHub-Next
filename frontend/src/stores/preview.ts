import { shallowRef } from "vue";
import type { DeliverySource } from "@/delivery/download";

export interface PreviewRequest {
  fileName: string;
  source: DeliverySource | null;
  downloadSource: DeliverySource | null;
  previewAllowed?: boolean;
  downloadAllowed?: boolean;
}
// 来源包含函数与私钥握手逻辑，不进入深层响应式代理。
export const previewRequest = shallowRef<PreviewRequest | null>(null);
export function openFilePreview(request: PreviewRequest): void {
  previewRequest.value = { ...request };
}
export function closeFilePreview(): void { previewRequest.value = null; }
