#!/usr/bin/env python3
"""Makes a crate build for Apple TV by treating tvOS like iOS.

Some crates in iroh's tree pick their Apple code with `target_os = "ios"`
and never name tvOS, so on Apple TV they compile with no platform code at
all. tvOS has the same system APIs as iOS for everything they use
(interfaces, routes, sockets, SystemConfiguration), so the fix is to let
the iOS code build there too. This script does that to a copy of the crate
under patches/, and points the workspace at the copy:

    python3 patches/add-tvos.py netwatch            the version in Cargo.lock
    python3 patches/add-tvos.py netwatch 0.19.3     a given version
    python3 patches/add-tvos.py netdev              a crate already in patches/: rewritten in place

Run it from the sidecar folder after `cargo fetch` (the copy comes from
Cargo's download cache). It changes:

  * every `target_os = "ios"` condition (in #[cfg], cfg!, cfg_attr,
    cfg_aliases! and the [target.'cfg(...)'] tables in Cargo.toml) into
    `any(target_os = "ios", target_os = "tvos")`, which is valid wherever the
    original was;
  * "ios" in build scripts' `matches!` patterns, so "ios" | "tvos".

Running it twice changes nothing more. Lines that mention "ios" in a way it
doesn't rewrite are listed at the end, to look at by hand.
"""

import os
import re
import shutil
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SIDECAR = os.path.dirname(HERE)
BOTH = 'any(target_os = "ios", target_os = "tvos")'
PRED = re.compile(r'target_os\s*=\s*"ios"')
# "ios" next to | in a match pattern (build.rs reading CARGO_CFG_TARGET_OS).
ARM_AFTER = re.compile(r'"ios"(?!\s*\|\s*"tvos")(\s*\|)')
ARM_BEFORE = re.compile(r'(\|\s*)"ios"(?!\s*\|\s*"tvos")')
ARM_LAST = re.compile(r'(\|\s*)"ios"(\s*(?:\)|=>))')
NOTE = "KINARA_PATCH.md"


def die(msg):
    sys.stderr.write("\n[add-tvos] " + msg + "\n\n")
    sys.exit(1)


def lock_version(name):
    """The crate's version in Cargo.lock, when there's exactly one."""
    try:
        text = open(os.path.join(SIDECAR, "Cargo.lock")).read()
    except OSError:
        return None
    found = re.findall(r'\[\[package\]\]\nname = "%s"\nversion = "([^"]+)"' % re.escape(name), text)
    if len(found) > 1:
        die("Cargo.lock has %s in more than one version (%s). Say which: add-tvos.py %s <version>" % (name, ", ".join(found), name))
    return found[0] if found else None


def registry_copy(name, version):
    """Cargo's unpacked source of name-version, or None."""
    home = os.environ.get("CARGO_HOME") or os.path.join(os.path.expanduser("~"), ".cargo")
    src = os.path.join(home, "registry", "src")
    if not os.path.isdir(src):
        return None
    for index in sorted(os.listdir(src)):
        path = os.path.join(src, index, "%s-%s" % (name, version))
        if os.path.isfile(os.path.join(path, "Cargo.toml")):
            return path
    return None


def rewrite_text(text, build_script):
    # Protect conditions already rewritten, so a second run leaves them.
    keep = "\x00KINARA_TVOS\x00"
    out = text.replace(BOTH, keep)
    out = PRED.sub(BOTH, out)
    out = out.replace(keep, BOTH)
    if build_script:
        out = ARM_LAST.sub(r'\1"ios" | "tvos"\2', out)
        out = ARM_AFTER.sub(r'"ios" | "tvos"\1', out)
        out = ARM_BEFORE.sub(r'\1"ios" | "tvos"', out)
    return out


def leftovers(text):
    """Lines still naming "ios" without tvOS next to them."""
    for i, line in enumerate(text.splitlines(), 1):
        if '"ios"' in line and '"tvos"' not in line:
            yield i, line.strip()


def rewrite_crate(folder):
    changed, left = [], []
    for dirpath, dirnames, files in os.walk(folder):
        dirnames[:] = [d for d in dirnames if d not in ("target", ".git")]
        for f in files:
            if not (f.endswith(".rs") or f == "Cargo.toml"):
                continue
            path = os.path.join(dirpath, f)
            with open(path, encoding="utf-8") as fh:
                text = fh.read()
            build_script = f == "build.rs" or os.sep + "build" + os.sep in path
            new = rewrite_text(text, build_script)
            rel = os.path.relpath(path, folder)
            if new != text:
                with open(path, "w", encoding="utf-8") as fh:
                    fh.write(new)
                changed.append(rel)
            for n, line in leftovers(new):
                left.append("%s:%d: %s" % (rel, n, line))
    return changed, left


def add_patch_entry(name):
    path = os.path.join(SIDECAR, "Cargo.toml")
    text = open(path).read()
    if re.search(r'(?m)^%s\s*=\s*\{\s*path\s*=\s*"patches/%s"' % (re.escape(name), re.escape(name)), text):
        return False
    if "[patch.crates-io]" not in text:
        text = text.rstrip("\n") + "\n\n[patch.crates-io]\n"
    text = text.rstrip("\n") + '\n%s = { path = "patches/%s" }\n' % (name, name)
    with open(path, "w") as fh:
        fh.write(text)
    return True


def main():
    args = sys.argv[1:]
    if not args or args[0] in ("-h", "--help"):
        print(__doc__)
        return
    name = args[0]
    dest = os.path.join(HERE, name)
    if os.path.isdir(dest):
        print("[add-tvos] patches/%s is already here: rewriting it in place." % name)
    else:
        version = args[1] if len(args) > 1 else lock_version(name)
        if not version:
            die("%s isn't in Cargo.lock. Give the version: add-tvos.py %s <version>" % (name, name))
        src = registry_copy(name, version)
        if not src:
            die("%s %s isn't in Cargo's download cache. Run `cargo fetch` in the sidecar folder first." % (name, version))
        shutil.copytree(src, dest, ignore=shutil.ignore_patterns(".cargo-ok", ".cargo_vcs_info.json", "target"))
        print("[add-tvos] Copied %s %s into patches/%s." % (name, version, name))
    changed, left = rewrite_crate(dest)
    note = os.path.join(dest, NOTE)
    if not os.path.exists(note):
        with open(note, "w") as fh:
            fh.write(
                "# Kinara patch\n\n"
                "This is the published crate with one change, made by `patches/add-tvos.py`:\n"
                "every place it picks iOS code also picks it on Apple TV (tvOS), which has\n"
                "the same system APIs for what this crate uses. Without it the crate builds\n"
                "with no platform code on Apple TV. Drop this copy (and its line under\n"
                "[patch.crates-io]) once the crate supports tvOS itself.\n"
            )
    added = add_patch_entry(name)
    print("[add-tvos] Rewrote %d file(s)%s" % (len(changed), ":" if changed else "."))
    for c in changed:
        print("    " + c)
    if added:
        print("[add-tvos] Added %s to [patch.crates-io] in Cargo.toml." % name)
    if left:
        print('\n[add-tvos] Still naming "ios" without tvOS (check these by hand):')
        for l in left:
            print("    " + l)


if __name__ == "__main__":
    main()
