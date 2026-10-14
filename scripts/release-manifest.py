#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path

root = Path('release-assets')
repo = os.environ['RELEASE_REPOSITORY']
tag = os.environ['RELEASE_TAG']
package = Path('npm/package.json')
pkg = json.loads(package.read_text())
if tag != 'v' + pkg['version']:
    raise SystemExit('Tag must match npm/package.json version')
# Linux ships the bare binary; macOS ships RepoGo Host.app, zipped, under apps,
# a key a host that predates the bundle never reads.
files = {'assets': {}, 'apps': {}}
for platform, kind, suffix in (('darwin', 'apps', '.app.zip'), ('linux', 'assets', '')):
    for arch in ('amd64', 'arm64'):
        name = f'repogo_{platform}_{arch}{suffix}'
        matches = list(root.rglob(name))
        if len(matches) != 1:
            raise SystemExit(f'Expected exactly one {name}')
        source = matches[0]
        digest = hashlib.sha256(source.read_bytes()).hexdigest()
        destination = root / name
        if source != destination:
            source.rename(destination)
        files[kind][f'{platform}-{arch}'] = {
            'name': name,
            'url': f'https://github.com/{repo}/releases/download/{tag}/{name}',
            'sha256': digest,
        }
for directory in sorted((p for p in root.rglob('*') if p.is_dir()), reverse=True):
    directory.rmdir()
manifest = {'version': pkg['version']}
for kind, entries in files.items():
    manifest[kind] = {key: {'url': f['url'], 'sha256': f['sha256']} for key, f in entries.items()}
(root / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
(root / 'checksums.txt').write_text(''.join(
    f'{f["sha256"]}  {f["name"]}\n' for entries in files.values() for f in entries.values()))
