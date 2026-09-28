import * as esbuild from 'esbuild';
import fs from 'node:fs';

const watch = process.argv.includes('--watch');
const production = process.argv.includes('--production');
// --web: only the web app (scripts/build-daemon.sh embeds it into vineyardd).
const webOnly = process.argv.includes('--web');

/** @type {esbuild.BuildOptions} */
const options = {
  entryPoints: ['src/extension/extension.ts'],
  bundle: true,
  outfile: 'dist/extension.js',
  platform: 'node',
  target: 'node20',
  format: 'cjs',
  external: ['vscode'],
  sourcemap: !production,
  minify: production,
  logLevel: 'info',
};

/** @type {esbuild.BuildOptions} */
const webview = {
  entryPoints: ['src/webview/main.ts'],
  bundle: true,
  outfile: 'dist/webview.js',
  platform: 'browser',
  target: 'es2022',
  format: 'iife',
  sourcemap: !production,
  minify: production,
  logLevel: 'info',
};

// The mobile web app, served by `vineyardd web` from files embedded in the binary.
const WEB_OUT = 'daemon/internal/web/static/build';
/** @type {esbuild.BuildOptions} */
const web = {
  entryPoints: ['src/web/app.ts', 'src/web/frame.ts'],
  bundle: true,
  outdir: WEB_OUT,
  platform: 'browser',
  target: ['es2022', 'safari16'],
  format: 'iife',
  loader: { '.ttf': 'file' },
  assetNames: '[name]',
  // Maps would be embedded in the daemon binary too; keep them to dev builds.
  sourcemap: !production,
  minify: production,
  logLevel: 'info',
  plugins: [
    {
      name: 'icon',
      setup(build) {
        build.onEnd(() => {
          fs.mkdirSync(WEB_OUT, { recursive: true });
          fs.copyFileSync('media/icon.png', `${WEB_OUT}/icon.png`);
        });
      },
    },
  ],
};

if (production) fs.rmSync(WEB_OUT, { recursive: true, force: true }); // no stale dev maps in a release

const builds = webOnly ? [web] : [options, webview, web];
if (watch) {
  const ctxs = await Promise.all(builds.map((b) => esbuild.context(b)));
  await Promise.all(ctxs.map((c) => c.watch()));
} else {
  await Promise.all(builds.map((b) => esbuild.build(b)));
}
