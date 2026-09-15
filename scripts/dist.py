#!/usr/bin/env python3
"""Build release binaries and installable archives for supported platforms."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import zipfile

version = sys.argv[1]
if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?", version):
    raise SystemExit("Version must be a semantic version without a v prefix")
root = Path(__file__).resolve().parents[1]
dist = root / "dist"
dist.mkdir(exist_ok=True)
checksums = []

def notices(environment):
    modules = subprocess.check_output([
        "go", "list", "-deps", "-f",
        "{{if .Module}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}", "."
    ], cwd=root, env=environment, text=True)
    blocks = []
    for entry in sorted(set(modules.splitlines())):
        if not entry:
            continue
        name, module_version, directory = entry.split("|", 2)
        if name == "github.com/ironicbadger/silo-plugin-tailscale":
            continue
        files = [p for p in Path(directory).iterdir() if p.is_file()
                 and p.name.upper().startswith(("LICENSE", "COPYING", "NOTICE", "COPYRIGHT"))]
        if not files:
            raise SystemExit(f"Missing dependency license: {name}")
        blocks.append(f"\n=== {name} {module_version} ===\n")
        for path in sorted(files):
            blocks.append(path.name + "\n" + path.read_text(errors="replace") + "\n")
    return "Third-party dependency notices\n" + "".join(blocks)

for goos, goarch in [("linux", "amd64"), ("linux", "arm64"), ("darwin", "arm64")]:
    platform = dist / f"{goos}-{goarch}"
    platform.mkdir(exist_ok=True)
    binary = platform / "plugin"
    environment = {**os.environ, "GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"}
    subprocess.run(["go", "build", "-trimpath", "-ldflags", f"-s -w -X main.version={version}", "-o", str(binary), "."],
                   cwd=root, env=environment, check=True)
    manifest = json.loads((root / "manifest.json").read_text())
    manifest["version"] = version
    manifest["checksum"] = hashlib.sha256(binary.read_bytes()).hexdigest()
    (platform / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    archive = dist / f"silo-plugin-tailscale-{version}-{goos}-{goarch}.zip"
    with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as bundle:
        for path in [binary, platform / "manifest.json", root / "LICENSE"]:
            bundle.write(path, arcname=path.name)
        bundle.write(root / ".build" / "tailscale" / "LICENSE", arcname="LICENSE.tailscale")
        bundle.writestr("THIRD_PARTY_NOTICES.txt", notices(environment))
    checksums.append(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}")
    print(archive.name)
(dist / "checksums.txt").write_text("\n".join(checksums) + "\n")
