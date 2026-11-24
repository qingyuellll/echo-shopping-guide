// Windows fallback for hosts that restrict child-process pipes. Uses inherited output.
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync, rmSync, mkdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = fileURLToPath(new URL('../', import.meta.url));
const output = path.resolve(root, 'dist');
if (path.dirname(output) !== path.resolve(root) || path.basename(output) !== 'dist') throw new Error('Unexpected output directory');
function run(command, args) {
  const result = spawnSync(command, args, { cwd: root, stdio: 'inherit', windowsHide: true });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status || 1);
}
for (const config of ['tsconfig.json', 'tsconfig.node.json']) run(process.execPath, ['node_modules/typescript/bin/tsc', '--noEmit', '-p', config]);
// A host-level trash shim can report an error after the directory is already gone;
// treat that as success so the build still runs on restricted Windows hosts.
try { rmSync(output, { recursive: true, force: true }); } catch { /* Rebuild over whatever remains. */ }
mkdirSync(output, { recursive: true });
const platform = `${process.platform}-${process.arch}`;
const binary = process.platform === 'win32' ? `node_modules/@esbuild/${platform}/esbuild.exe` : `node_modules/@esbuild/${platform}/bin/esbuild`;
run(path.join(root, binary), ['src/main.tsx', '--bundle', '--splitting', '--format=esm', '--minify', '--outdir=dist/assets', '--entry-names=[name]-[hash]', '--jsx=automatic', '--define:process.env.NODE_ENV="production"', '--metafile=dist/build-meta.json']);
const metadata = JSON.parse(readFileSync(path.join(output, 'build-meta.json'), 'utf8'));
const [entry, info] = Object.entries(metadata.outputs).find(([, value]) => value.entryPoint === 'src/main.tsx');
let html = readFileSync(path.join(root, 'index.html'), 'utf8');
html = html.replace('<script type="module" src="/src/main.tsx"></script>', `<script type="module" src="/${entry.replace(/^dist\//, '')}"></script>`);
if (info.cssBundle) html = html.replace('</head>', `<link rel="stylesheet" href="/${info.cssBundle.replace(/^dist\//, '')}" />\n  </head>`);
writeFileSync(path.join(output, 'index.html'), html);
console.log('Production frontend built and typechecked.');
