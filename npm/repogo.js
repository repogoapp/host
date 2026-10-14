#!/usr/bin/env node
import { chmod, mkdir, mkdtemp, readFile, rename, rm, symlink, unlink, writeFile } from 'node:fs/promises';
import { createHash, createPublicKey, randomUUID, verify } from 'node:crypto';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { spawn, spawnSync } from 'node:child_process';

// publicKey checks manifest.json.sig. The release workflow signs with its
// private half; internal/release/release.go holds the same key.
const publicKey = createPublicKey({
  key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from('XaWEBib5lq+jel0GkUaC8amZ/87MB7WYL32FEaQBm/Q=', 'base64').toString('base64url') },
  format: 'jwk',
});

async function download(url) {
  if (!url.startsWith('https://')) throw new Error('Release URL must use HTTPS');
  const response = await fetch(url, { signal: AbortSignal.timeout(120_000) });
  if (!response.ok) throw new Error(`Download failed: ${response.status} ${response.statusText}`);
  return response;
}

// installedVersion is what the binary reports, or null when it is missing or won't run.
function installedVersion(binary) {
  const result = spawnSync(binary, ['version'], { encoding: 'utf8', timeout: 10_000 });
  return result.status === 0 ? result.stdout.trim() : null;
}

// older reports whether version a is below b; anything unreadable counts as older.
function older(a, b) {
  const parse = v => (/^\d+\.\d+\.\d+$/.test(v ?? '') ? v.split('.').map(Number) : null);
  const [x, y] = [parse(a), parse(b)];
  if (!x) return true;
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] < y[i];
  return false;
}

// installApp unzips RepoGo Host.app into home, swaps it in, and points binary at
// the repogo inside it. A running host keeps the bundle it started from.
async function installApp(bytes, home, binary) {
  const app = join(home, 'RepoGo Host.app');
  const staging = await mkdtemp(join(home, '.download-'));
  try {
    const zip = join(staging, 'app.zip');
    await writeFile(zip, bytes);
    const unzip = spawnSync('/usr/bin/ditto', ['-x', '-k', zip, staging], { encoding: 'utf8' });
    if (unzip.status !== 0) throw new Error(`Could not unpack RepoGo Host.app: ${unzip.stderr.trim()}`);
    await rename(app, join(staging, 'previous.app')).catch(error => { if (error.code !== 'ENOENT') throw error; });
    await rename(join(staging, 'RepoGo Host.app'), app);
    const link = join(dirname(binary), `.link-${randomUUID()}`);
    await symlink(join('..', 'RepoGo Host.app', 'Contents', 'MacOS', 'repogo'), link);
    // rename replaces a bare binary from an older release, or the old link, in one step.
    await rename(link, binary);
  } finally {
    await rm(staging, { recursive: true, force: true });
  }
}

// installBinary writes a bare binary to binary in one step.
async function installBinary(bytes, binary) {
  const temporary = join(dirname(binary), `.download-${randomUUID()}`);
  try {
    await writeFile(temporary, bytes, { mode: 0o755, flag: 'wx' });
    await chmod(temporary, 0o755);
    // rename swaps the file in one step; a running host keeps the binary it started with.
    await rename(temporary, binary);
  } finally {
    await unlink(temporary).catch(error => { if (error.code !== 'ENOENT') throw error; });
  }
}

try {
  const home = join(homedir(), '.repogo');
  const dir = join(home, 'bin');
  const binary = join(dir, 'repogo');
  const pkg = JSON.parse(await readFile(new URL('./package.json', import.meta.url), 'utf8'));
  // A newer binary from `repogo update` stays; only a missing or older one is replaced.
  if (older(installedVersion(binary), pkg.version)) {
    if (!pkg.releaseRepository) throw new Error('This package has no release repository configured');
    const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch];
    const base = `https://github.com/${pkg.releaseRepository}/releases/download/v${pkg.version}/`;
    const body = Buffer.from(await (await download(base + 'manifest.json')).arrayBuffer());
    const signature = Buffer.from(await (await download(base + 'manifest.json.sig')).arrayBuffer());
    // The checksums below are only as good as the manifest, so its signature comes first.
    if (!verify(null, body, publicKey, signature)) throw new Error('Release signature mismatch');
    const manifest = JSON.parse(body);
    if (manifest.version !== pkg.version) throw new Error('Release version mismatch');
    // macOS gets RepoGo Host.app, so System Settings shows the host's name and icon.
    const macOS = process.platform === 'darwin';
    const asset = (macOS ? manifest.apps : manifest.assets)?.[`${process.platform}-${arch}`];
    if (!asset) throw new Error(`Unsupported platform: ${process.platform}/${process.arch}`);
    const bytes = Buffer.from(await (await download(asset.url)).arrayBuffer());
    if (createHash('sha256').update(bytes).digest('hex') !== asset.sha256) throw new Error('Release checksum mismatch');
    await mkdir(dir, { recursive: true, mode: 0o700 });
    if (macOS) await installApp(bytes, home, binary);
    else await installBinary(bytes, binary);
  }
  const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(signal, () => child.kill(signal));
  child.on('error', error => { console.error(`repogo: ${error.message}`); process.exitCode = 1; });
  child.on('exit', (code, signal) => { process.exitCode = code ?? (signal === 'SIGINT' ? 130 : 1); });
} catch (error) {
  console.error(`repogo: ${error.message}`);
  process.exitCode = 1;
}
