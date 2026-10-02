#!/usr/bin/env node
import { chmod, mkdir, readFile, rename, unlink, writeFile } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { spawn, spawnSync } from 'node:child_process';

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

try {
  const dir = join(homedir(), '.repogo', 'bin');
  const binary = join(dir, 'repogo');
  const pkg = JSON.parse(await readFile(new URL('./package.json', import.meta.url), 'utf8'));
  // A newer binary from `repogo update` stays; only a missing or older one is replaced.
  if (older(installedVersion(binary), pkg.version)) {
    if (!pkg.releaseRepository) throw new Error('This package has no release repository configured');
    const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch];
    const url = `https://github.com/${pkg.releaseRepository}/releases/download/v${pkg.version}/manifest.json`;
    const manifest = await (await download(url)).json();
    if (manifest.version !== pkg.version) throw new Error('Release version mismatch');
    const asset = manifest.assets[`${process.platform}-${arch}`];
    if (!asset) throw new Error(`Unsupported platform: ${process.platform}/${process.arch}`);
    const bytes = Buffer.from(await (await download(asset.url)).arrayBuffer());
    if (createHash('sha256').update(bytes).digest('hex') !== asset.sha256) throw new Error('Release checksum mismatch');
    await mkdir(dir, { recursive: true, mode: 0o700 });
    const temporary = join(dir, `.download-${randomUUID()}`);
    try {
      await writeFile(temporary, bytes, { mode: 0o755, flag: 'wx' });
      await chmod(temporary, 0o755);
      // rename swaps the file in one step; a running host keeps the binary it started with.
      await rename(temporary, binary);
    } finally {
      await unlink(temporary).catch(error => { if (error.code !== 'ENOENT') throw error; });
    }
  }
  const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(signal, () => child.kill(signal));
  child.on('error', error => { console.error(`repogo: ${error.message}`); process.exitCode = 1; });
  child.on('exit', (code, signal) => { process.exitCode = code ?? (signal === 'SIGINT' ? 130 : 1); });
} catch (error) {
  console.error(`repogo: ${error.message}`);
  process.exitCode = 1;
}
