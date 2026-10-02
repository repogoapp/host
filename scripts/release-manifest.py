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
assets = {}
for platform in ('darwin', 'linux'):
    for arch in ('amd64', 'arm64'):
        name = f'repogo_{platform}_{arch}'
        matches = list(root.rglob(name))
        if len(matches) != 1:
            raise SystemExit(f'Expected exactly one {name}')
        source = matches[0]
        digest = hashlib.sha256(source.read_bytes()).hexdigest()
        destination = root / name
        if source != destination:
            source.rename(destination)
        assets[f'{platform}-{arch}'] = {
            'url': f'https://github.com/{repo}/releases/download/{tag}/{name}',
            'sha256': digest,
        }
for directory in sorted((p for p in root.rglob('*') if p.is_dir()), reverse=True):
    directory.rmdir()
(root / 'manifest.json').write_text(json.dumps({'version': pkg['version'], 'assets': assets}, indent=2) + '\n')
(root / 'checksums.txt').write_text(''.join(
    f'{asset["sha256"]}  repogo_{key.replace("-", "_")}\n'
    for key, asset in assets.items()))
