import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import ts from "typescript";

const source = await readFile(new URL("../src/lib/droppedFiles.ts", import.meta.url), "utf8");
const compiled = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 },
}).outputText;
const moduleUrl = `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`;
const { collectDroppedFiles } = await import(moduleUrl);

const mockFile = name => ({ name, size: 1 });
function fileEntry(name) {
  const file = mockFile(name);
  return { name, isFile: true, isDirectory: false, file: success => success(file) };
}
function directoryEntry(name, batches) {
  let index = 0;
  return {
    name,
    isFile: false,
    isDirectory: true,
    createReader: () => ({ readEntries: success => success(batches[index++] ?? []) }),
  };
}

test("递归收集拖入目录的文件并保留目录层级与空目录", async () => {
  const nested = directoryEntry("nested", [[fileEntry("b.txt")], []]);
  const empty = directoryEntry("empty", [[]]);
  const root = directoryEntry("photos", [[fileEntry("a.txt"), nested, empty], []]);
  const item = { kind: "file", webkitGetAsEntry: () => root, getAsFile: () => null };

  const result = await collectDroppedFiles([item], []);
  const paths = new Map(result.files.map((file, index) => [file.name, result.relativePaths[index]]));
  assert.deepEqual(paths, new Map([["a.txt", "photos/a.txt"], ["b.txt", "photos/nested/b.txt"]]));
  assert.deepEqual(new Set(result.directories), new Set(["photos", "photos/nested", "photos/empty"]));
});

test("普通文件拖入使用 DataTransfer.files 兜底", async () => {
  const file = mockFile("plain.txt");
  const result = await collectDroppedFiles([], [file]);
  assert.deepEqual(result.files, [file]);
  assert.deepEqual(result.relativePaths, ["plain.txt"]);
  assert.deepEqual(result.directories, []);
});

test("entry 异步返回空内容时仍使用 drop 同步快照的 FileList", async () => {
  const fallbackFile = mockFile("fallback.txt");
  let available = true;
  const fallbackFiles = {
    get length() { return available ? 1 : 0; },
    get 0() { return available ? fallbackFile : undefined; },
  };
  const emptyRoot = directoryEntry("empty", [[]]);
  const item = { kind: "file", webkitGetAsEntry: () => emptyRoot, getAsFile: () => null };

  const pending = collectDroppedFiles([item], fallbackFiles);
  available = false;
  const result = await pending;
  assert.deepEqual(result.files, [fallbackFile]);
  assert.deepEqual(result.relativePaths, ["fallback.txt"]);
});

test("目录 entry 读取失败且未产出文件时回退到快照文件列表", async () => {
  const fallbackFile = mockFile("fallback.txt");
  const brokenRoot = {
    name: "broken",
    isFile: false,
    isDirectory: true,
    createReader: () => ({ readEntries: (_success, failure) => failure(new Error("unreadable")) }),
  };
  const item = { kind: "file", webkitGetAsEntry: () => brokenRoot, getAsFile: () => null };

  const result = await collectDroppedFiles([item], [fallbackFile]);
  assert.deepEqual(result.files, [fallbackFile]);
  assert.deepEqual(result.relativePaths, ["fallback.txt"]);
});
