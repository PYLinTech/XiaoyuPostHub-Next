export function isByteTransfer(status: string, phase: string): boolean {
  return status === "running" && ["uploading", "fetching", "decrypting"].includes(phase);
}

/** Only callers' currently transferring tasks participate; preparation and finalization are excluded. */
export function aggregateTransferProgress(tasks: readonly { done: number; total: number }[]): number | null {
  let done = 0, total = 0;
  for (const task of tasks) {
    if (!Number.isFinite(task.total) || task.total <= 0) continue;
    total += task.total;
    done += Math.min(task.total, Math.max(0, Number.isFinite(task.done) ? task.done : 0));
  }
  return total > 0 ? Math.min(.99, Math.floor(done / total * 100) / 100) : null;
}
