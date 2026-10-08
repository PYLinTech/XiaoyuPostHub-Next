import { Sha256, bytesToHex } from "@/crypto/sha256";

const scope = self as DedicatedWorkerGlobalScope;

scope.onmessage = async (event: MessageEvent<{ file: File }>) => {
  const { file } = event.data;
  const chunkSize = 4 * 1024 * 1024;
  const hasher = new Sha256();
  try {
    for (let offset = 0; offset < file.size; offset += chunkSize) {
      const buffer = await file.slice(offset, Math.min(offset + chunkSize, file.size)).arrayBuffer();
      hasher.update(new Uint8Array(buffer));
      scope.postMessage({ type: "progress", processed: Math.min(offset + buffer.byteLength, file.size), total: file.size });
    }
    scope.postMessage({ type: "done", checksum: bytesToHex(hasher.digest()) });
  } catch (error) {
    scope.postMessage({ type: "error", message: error instanceof Error ? error.message : "校验失败" });
  }
};
