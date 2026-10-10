/** iPhone Safari 部分版本仅提供 ManagedMediaSource。 */
export function mediaSourceConstructor(): typeof MediaSource | undefined {
  const scope = globalThis as typeof globalThis & { ManagedMediaSource?: typeof MediaSource };
  return scope.MediaSource || scope.ManagedMediaSource;
}

export function supportsMediaSourceType(mime: string): boolean {
  return mediaSourceConstructor()?.isTypeSupported(mime) || false;
}
