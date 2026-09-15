#!/usr/bin/env python3
"""Apply the two file-free storage adaptations to the pinned Tailscale source.

The module cache is never modified. Exact matches fail closed on upstream drift.
"""
import json
import pathlib
import subprocess
import shutil

root = pathlib.Path(__file__).resolve().parents[1]
module = json.loads(subprocess.check_output(
    ["go", "mod", "download", "-json", "tailscale.com"], cwd=root))
if module["Version"] != "v1.102.4":
    raise SystemExit("Review the storage adaptations before updating Tailscale")
source = pathlib.Path(module["Dir"])
output = root / ".build" / "tailscale"
if not output.exists():
    shutil.copytree(source, output)
for path in output.rglob("*"):
    path.chmod(0o755 if path.is_dir() else 0o644)

def patch(path, edits):
    original = source / path
    text = original.read_text()
    for before, after in edits:
        if text.count(before) != 1:
            raise SystemExit(f"Upstream drift in {path}: {before[:60]!r}")
        text = text.replace(before, after, 1)
    target = output / path
    target.write_text(text)

patch("tsnet/tsnet.go", [
    ("\ts.rootPath = s.Dir", "\ts.rootPath = s.Dir\n\tif s.NoLocalState { s.rootPath = \"\" }"),
    ("type Server struct {", "type Server struct {\n\t// NoLocalState requires an external Store and disables local files and logtail.\n\tNoLocalState bool\n"),
    ('\tif s.rootPath == "" {\n\t\tconfDir', '\tif s.NoLocalState && s.Store == nil {\n\t\treturn errors.New("NoLocalState requires an external Store")\n\t}\n\tif !s.NoLocalState {\n\tif s.rootPath == "" {\n\t\tconfDir'),
    ('\n\ttsLogf := func(format string, a ...any) {', '\n\t}\n\n\ttsLogf := func(format string, a ...any) {'),
    ('\tif testenv.InTest() {\n\t\treturn nil\n\t}\n\tcfgPath', '\tif s.NoLocalState || testenv.InTest() {\n\t\treturn nil\n\t}\n\tcfgPath'),
])
patch("feature/acme/certstore.go", [
    ('\tdefault:\n\t\tif hostinfo.GetEnvType() == hostinfo.Kubernetes {\n\t\t\t// We\'re running in Kubernetes with a custom StateStore,\n\t\t\t// use that instead of the cert directory.\n\t\t\t// TODO(maisem): expand this to other environments?\n\t\t\treturn certStateStore{StateStore: st}, nil\n\t\t}', '\tdefault:\n\t\treturn certStateStore{StateStore: st}, nil'),
    ('\n\t"tailscale.com/hostinfo"', ''),
])
(root / ".build" / "go.mod").write_text((root / "go.mod").read_text() + "\nreplace tailscale.com => ./.build/tailscale\n")
shutil.copyfile(root / "go.sum", root / ".build" / "go.sum")
shutil.copyfile(root / "scripts" / "acme_state_test.go.txt", output / "feature" / "acme" / "silo_state_test.go")
