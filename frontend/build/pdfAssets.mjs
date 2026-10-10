import { cp, mkdir, readFile } from 'node:fs/promises';
import { resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

// PDF 静态资源不需要监听或 glob 依赖，开发期按需读取，构建时直接复制。
export function pdfAssetsPlugin() {
  const root = resolve(fileURLToPath(new URL('../node_modules/pdfjs-dist/', import.meta.url)));
  let outDir;
  return {
    name: 'xph-pdf-assets',
    configResolved(config) { outDir = resolve(config.root, config.build.outDir, 'pdfjs'); },
    configureServer(server) {
      server.middlewares.use(async (req, res, next) => {
        const raw = (req.url ?? '').split('?')[0];
        if (!raw.startsWith('/pdfjs/')) return next();
        try {
          const name = decodeURIComponent(raw.slice('/pdfjs/'.length));
          if (name.split('/').some(part => part === '..' || part === '.') || name.includes('\\')) {
            res.statusCode = 400; res.end(); return;
          }
          const relative = name === 'pdf.worker.min.mjs' ? 'legacy/build/' + name : name;
          if (name !== 'pdf.worker.min.mjs' && !/^(cmaps|standard_fonts|wasm)\/.+/.test(name)) {
            res.statusCode = 404; res.end(); return;
          }
          const path = resolve(root, relative);
          if (!path.startsWith(root + sep)) { res.statusCode = 400; res.end(); return; }
          const body = await readFile(path);
          res.setHeader('Content-Type', name.endsWith('.mjs') ? 'text/javascript' : name.endsWith('.wasm') ? 'application/wasm' : 'application/octet-stream');
          res.setHeader('Content-Length', body.length);
          res.end(req.method === 'HEAD' ? undefined : body);
        } catch (err) {
          res.statusCode = err instanceof URIError ? 400 : err.code === 'ENOENT' || err.code === 'EISDIR' ? 404 : 500;
          res.end();
        }
      });
    },
    async closeBundle() {
      await mkdir(outDir, { recursive: true });
      await cp(resolve(root, 'legacy/build/pdf.worker.min.mjs'), resolve(outDir, 'pdf.worker.min.mjs'));
      for (const name of ['cmaps', 'standard_fonts', 'wasm']) {
        await cp(resolve(root, name), resolve(outDir, name), { recursive: true });
      }
    },
  };
}
