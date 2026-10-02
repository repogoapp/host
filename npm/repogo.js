#!/usr/bin/env node
import { access, chmod, link, mkdir, readFile, unlink, writeFile } from 'node:fs/promises';
import { createHash, randomUUID } from 'node:crypto';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { spawn } from 'node:child_process';

async function download(url) {
  if (!url.startsWith('https://')) throw new Error('Release URL must use HTTPS');
  const response = await fetch(url, { signal: AbortSignal.timeout(120_000) });
  if (!response.ok) throw new Error(`Download failed: ${response.status} ${response.statusText}`);
  return response;
}

try {
  const dir = join(homedir(), '.repogo', 'bin');
  const binary = join(dir, 'repogo');
  let installed = false;
  try { await access(binary); installed = true; }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (!installed) {
    const pkg = JSON.parse(await readFile(new URL('./package.json', import.meta.url), 'utf8'));
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
      try { await link(temporary, binary); }
      catch (error) { if (error.code !== 'EEXIST') throw error; }
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
