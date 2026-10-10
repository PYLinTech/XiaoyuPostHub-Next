import { ApiError } from "@/api/client";
import type { DeliveryProgress, DeliveryResult } from "./download";
import type { UploadProgressInfo, UploadOutcome } from "./upload";
import type { ConflictAction } from "@/api/types";

export interface TransferError { name: string; message: string; status?: number; detail?: string; retryAfter?: number }
export function packError(error: unknown): TransferError {
  if (error instanceof ApiError) return { name: error.name, message: error.message, status: error.status, detail: error.detail, retryAfter: error.retryAfter };
  return error instanceof Error || error instanceof DOMException
    ? { name: error.name, message: error.message } : { name: "Error", message: String(error) };
}
export function unpackError(error: TransferError): Error {
  if (error.status !== undefined) return new ApiError(error.status, error.message, error.detail, error.retryAfter);
  if (error.name === "AbortError") return new DOMException(error.message, error.name);
  return Object.assign(new Error(error.message), { name: error.name });
}
export type Start = { type: "start"; token: string } & (
  { kind: "upload"; file: File; parentPath: string; conflictAction?: ConflictAction; concurrency?: number; sessions: [string, string][] } |
  { kind: "download"; streamToDiskAbove?: number }
);
export type HostMessage = Start | { type: "cancel" } | { type: "reply"; id: number; value?: unknown; error?: TransferError };
export type WorkerMessage =
  | { type: "progress"; value: DeliveryProgress | UploadProgressInfo }
  | { type: "session"; value: string }
  | { type: "storage"; key: string; value: string | null }
  | { type: "unauthorized"; token: string }
  | { type: "request"; id: number; method: "plan" | "open" | "write" | "close" | "abort"; value?: unknown }
  | { type: "result"; value: DeliveryResult | UploadOutcome }
  | { type: "error"; error: TransferError };
