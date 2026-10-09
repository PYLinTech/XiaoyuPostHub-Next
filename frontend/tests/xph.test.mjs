import assert from "node:assert/strict";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { decryptAll, parseXphHeader } from "../src/crypto/xph.ts";

async function fixture(blockCount = 12) {
  const raw = new Uint8Array(64);
  raw.set(new TextEncoder().encode("XPHCRPT1"));
  raw[8] = 1;
  raw[9] = 9;
  new DataView(raw.buffer).setBigUint64(28, BigInt(blockCount * 512), true);
  const header = parseXphHeader(raw);
  const key = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
  const blocks = await Promise.all(Array.from({ length: blockCount }, async (_, index) => {
    const iv = new Uint8Array(12);
    new DataView(iv.buffer).setBigUint64(4, BigInt(index), false);
    return new Uint8Array(await crypto.subtle.encrypt(
      { name: "AES-GCM", iv, additionalData: header.aad }, key, new Uint8Array(512).fill(index),
    ));
  }));
  const cipher = new Uint8Array(header.cipherSize);
  cipher.set(raw);
  blocks.forEach((block, index) => cipher.set(block, 64 + index * 528));
  return { key, header, read: (start, end) => cipher.slice(start, end) };
}

test("decryption bounds read-ahead while delivering ordered blocks to a slow sink", async () => {
  const { key, header, read } = await fixture();
  const delivered = [];
  let started = 0;
  await decryptAll(key, header, async (start, end) => {
    started++;
    assert.ok(started - delivered.length <= 3, "read-ahead exceeds concurrency window");
    if (start === 64) await delay(20);
    return read(start, end);
  }, async (plain, index) => {
    assert.equal(index, delivered.length);
    assert.equal(plain[0], index);
    await delay(2);
    delivered.push(index);
  }, { concurrency: 3, batchBlocks: 1 });
  assert.equal(delivered.length, header.blockCount);
});

test("sink failure preserves its error and cancels in-flight ranges", async () => {
  const { key, header, read } = await fixture();
  const failure = new Error("disk write failed");
  let aborted = false;
  await assert.rejects(decryptAll(key, header, async (start, end, signal) => {
    if (start === 64) return read(start, end);
    try { await delay(1000, undefined, { signal }); }
    catch (error) { aborted = true; throw error; }
    return read(start, end);
  }, () => { throw failure; }, { concurrency: 3, batchBlocks: 1 }), (error) => error === failure);
  assert.ok(aborted);
});

test("read failure waits for the active sink write and stops subsequent delivery", async () => {
  const { key, header, read } = await fixture();
  const failure = new Error("range failed");
  let entered;
  const writing = new Promise((resolve) => { entered = resolve; });
  let writes = 0;
  let writeFinished = false;
  await assert.rejects(decryptAll(key, header, async (start, end) => {
    if (start === 64) return read(start, end);
    await writing;
    throw failure;
  }, async () => {
    writes++;
    entered();
    await delay(20);
    writeFinished = true;
  }, { concurrency: 2, batchBlocks: 1 }), (error) => error === failure);
  assert.ok(writeFinished, "rejection must not race an active disk write");
  assert.equal(writes, 1);
});
