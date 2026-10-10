import { readFile } from "node:fs/promises";
import { fileURLToPath, URL } from "node:url";
import { defineConfig, type Plugin } from "vite";
import vue from "@vitejs/plugin-vue";
import { pdfAssetsPlugin } from "./build/pdfAssets.mjs";
import { build as esbuild } from "esbuild";
import { adaptPreviewBundle, previewMarkdownModule } from "./build/previewCompatibility.mjs";

// Service Worker 单独打进一个自包含文件。
//
// 不放进 rollup 的多入口，是因为多入口会把与主包共享的代码拆成带哈希的公共 chunk：
// 那样 SW 的更新检测会漏掉"只有被导入的 chunk 变了"这种情况，导致修好的解密逻辑
// 在用户机器上不生效。用 esbuild 打成单文件就没有这个隐患。
//
// 开发期也照样产出并由中间件直接吐出来，这样"经 SW 实时解密预览"这条主路径
// 在开发时就是活的，不会等到上线才第一次运行。
function serviceWorkerPlugin(): Plugin {
  const entry = fileURLToPath(new URL("./src/sw.ts", import.meta.url));
  const alias = { "@": fileURLToPath(new URL("./src", import.meta.url)) };
  const devOutfile = fileURLToPath(new URL("./node_modules/.xph/xph-sw.js", import.meta.url));

  const compile = (outfile: string) =>
    esbuild({
      entryPoints: [entry],
      outfile,
      bundle: true,
      format: "esm",
      target: "es2022",
      legalComments: "none",
      alias,
    });

  return {
    name: "xph-service-worker",
    async configureServer(server) {
      server.middlewares.use(async (req, res, next) => {
        const path = (req.url ?? "").split("?")[0];
        if (path !== "/xph-sw.js") {
          return next();
        }
        try {
          // 每次请求都重新编译：改完 SW 刷新页面即刻生效，不必重启 dev server。
          await compile(devOutfile);
          const body = await readFile(devOutfile);
          res.setHeader("Content-Type", "text/javascript; charset=utf-8");
          // 允许 SW 作用到根路径，否则它只能拦截 /assets 下的请求。
          res.setHeader("Service-Worker-Allowed", "/");
          res.setHeader("Cache-Control", "no-store");
          res.end(body);
        } catch (err) {
          res.statusCode = 500;
          res.end(String(err));
        }
      });
    },
    async closeBundle() {
      await compile(fileURLToPath(new URL("./dist/xph-sw.js", import.meta.url)));
    },
  };
}

function localPreviewWorkerPlugin(worker = false): Plugin {
  return {
    name: "xph-local-preview-worker",
    enforce: "pre",
    transform: worker ? undefined : adaptPreviewBundle,
    resolveId(id) { if (id === "virtual:xph-preview-markdown") return "\0xph-preview-markdown"; },
    async load(id) {
      if (id !== "\0xph-preview-markdown") return;
      return previewMarkdownModule(await readFile(fileURLToPath(new URL(
        "./node_modules/@eternalheart/vue-file-preview/lib/chunks/index-B6pWnG9T.mjs", import.meta.url)), "utf8"));
    },
  };
}

// 开发期把 /api 代理到后端，生产构建的产物由后端直接托管（dist 目录）。
// 之所以不做成"前端另起一个服务"，是因为交付链路里票据与密钥都依赖同源：
// 跨源会让密钥下发通道失去意义，也会让 Service Worker 无法注册。
export default defineConfig({
  plugins: [vue(), serviceWorkerPlugin(), localPreviewWorkerPlugin(), pdfAssetsPlugin()],
  worker: { format: "es", plugins: () => [localPreviewWorkerPlugin(true)] },
  optimizeDeps: { exclude: ["@eternalheart/vue-file-preview", "pptx-preview"] },
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
      "@preview-core": fileURLToPath(new URL("./node_modules/@eternalheart/vue-file-preview/lib/chunks/index-CyaXSkiB.mjs", import.meta.url)),
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: process.env.XPH_BACKEND ?? "http://127.0.0.1:8080",
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
    // 解密与上传都在浏览器里跑，源码映射会让产物翻倍；排障时临时打开即可。
    sourcemap: false,
    target: "es2022",
  },
});
