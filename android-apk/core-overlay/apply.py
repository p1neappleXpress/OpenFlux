#!/usr/bin/env python3
"""Apply Android client changes to an isolated, pinned OpenFlux build copy."""
import pathlib
import re
import shutil
import sys

if len(sys.argv) != 2:
    raise SystemExit('usage: apply.py OPENFLUX_BUILD_COPY')
root = pathlib.Path(sys.argv[1]).resolve()
overlay = pathlib.Path(__file__).resolve().parent
endpoint = root / 'tunnel/endpoint.go'
text = endpoint.read_text()
changes = [
    ('type TunnelLinkEndpoint struct {', 'type TunnelLinkEndpoint struct {\n\tmtu uint32'),
    ('return &TunnelLinkEndpoint{}', 'return &TunnelLinkEndpoint{mtu: openFluxLinkMTU()}'),
]
for old, new in changes:
    if text.count(old) != 1:
        raise SystemExit('Unexpected OpenFlux endpoint.go; cannot apply MTU patch safely')
    text = text.replace(old, new, 1)
pattern = r'(func \(e \*TunnelLinkEndpoint\) MTU\(\) uint32\s*\{) return 1500 (\})'
text, count = re.subn(pattern, r'\1 return e.mtu \2', text)
if count != 1:
    raise SystemExit('Expected the pinned MTU method in OpenFlux endpoint.go')
endpoint.write_text(text)
for name in ('socks5.go', 'socks5_test.go'):
    shutil.copy2(overlay / name, root / 'socks5' / name)
for p in (overlay / 'tunnel').glob('*.go'):
    shutil.copy2(p, root / 'tunnel' / p.name)
