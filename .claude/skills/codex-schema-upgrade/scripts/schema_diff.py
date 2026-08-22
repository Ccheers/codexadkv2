#!/usr/bin/env python3
"""Diff two versions of the Codex app-server JSON Schema.

Every schema upgrade starts with the same question: what actually changed? This
answers it in one command instead of hand-rolling a comparison each time.

Two modes:

  # What does the pending sync change? (committed schema vs working tree)
  schema_diff.py

  # Compare two schema directories explicitly
  schema_diff.py --old /tmp/old-schemas --new internal/schemas

By default it compares the git HEAD version of internal/schemas against the
working tree, which is exactly the state right after `make sync-schemas`.

Exit status is 0 even when differences exist; non-zero means the diff itself
could not be computed.
"""

import argparse
import json
import subprocess
import sys
from pathlib import Path

# The files that make up one vendored schema variant. The two bundles hold type
# definitions; the four union files hold the method tables.
BUNDLES = [
    "codex_app_server_protocol.schemas.json",
    "codex_app_server_protocol.v2.schemas.json",
]
METHOD_FILES = [
    "ClientRequest.json",
    "ServerRequest.json",
    "ServerNotification.json",
    "ClientNotification.json",
]


def load_json(text, label):
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        sys.exit(f"error: {label} is not valid JSON: {exc}")


def read_from_dir(directory, name):
    path = Path(directory) / name
    if not path.exists():
        return None
    return load_json(path.read_text(), str(path))


def read_from_git(ref, directory, name):
    """Read a file as of a git ref, or None when it did not exist then."""
    target = f"{ref}:{Path(directory).as_posix()}/{name}"
    result = subprocess.run(["git", "show", target], capture_output=True, text=True)
    if result.returncode != 0:
        return None
    return load_json(result.stdout, target)


def definitions(reader):
    """Merge both bundles into one definition namespace, as the generator does."""
    out = {}
    for bundle in BUNDLES:
        doc = reader(bundle)
        if doc:
            out.update(doc.get("definitions", {}))
    return out


def methods(reader, filename):
    """Map method name -> params type name for one union file."""
    doc = reader(filename)
    if not doc:
        return {}
    out = {}
    for variant in doc.get("oneOf", []):
        props = variant.get("properties", {})
        method = props.get("method", {}).get("enum", [None])[0]
        if not method:
            continue
        params = props.get("params") or {}
        ref = params.get("$ref", "")
        if not ref:
            # anyOf[{$ref}, null] means the params are optional.
            for arm in (params.get("anyOf") or params.get("oneOf") or []):
                if arm.get("$ref"):
                    ref = arm["$ref"]
                    break
        out[method] = ref.split("/")[-1] if ref else "(none)"
    return out


def section(title):
    print()
    print(title)
    print("-" * len(title))


def report_set(label, items, detail=None):
    if not items:
        return False
    section(f"{label} ({len(items)})")
    for item in sorted(items):
        suffix = f"  -> {detail[item]}" if detail and item in detail else ""
        print(f"  {item}{suffix}")
    return True


def main():
    parser = argparse.ArgumentParser(
        description="Diff two versions of the vendored Codex protocol schema."
    )
    parser.add_argument(
        "--dir",
        default="internal/schemas",
        help="schema directory in the working tree (default: internal/schemas)",
    )
    parser.add_argument(
        "--ref", default="HEAD", help="git ref to compare against (default: HEAD)"
    )
    parser.add_argument("--old", help="explicit old schema directory")
    parser.add_argument("--new", help="explicit new schema directory")
    parser.add_argument(
        "--variant",
        choices=["experimental", "stable"],
        default="experimental",
        help="which vendored variant to compare (default: experimental)",
    )
    args = parser.parse_args()

    prefix = "" if args.variant == "experimental" else "stable-"

    if args.old and args.new:
        old_reader = lambda n: read_from_dir(args.old, prefix + n)  # noqa: E731
        new_reader = lambda n: read_from_dir(args.new, prefix + n)  # noqa: E731
        old_label, new_label = args.old, args.new
        old_version = read_version_dir(args.old)
    else:
        old_reader = lambda n: read_from_git(args.ref, args.dir, prefix + n)  # noqa: E731
        new_reader = lambda n: read_from_dir(args.dir, prefix + n)  # noqa: E731
        old_label, new_label = f"{args.ref}:{args.dir}", args.dir
        old_version = read_version_git(args.ref, args.dir)

    new_version = read_version_dir(args.new or args.dir)

    print(f"Comparing {args.variant} schema")
    print(f"  old: {old_label} (codex-cli {old_version})")
    print(f"  new: {new_label} (codex-cli {new_version})")

    old_defs = definitions(old_reader)
    new_defs = definitions(new_reader)
    if not old_defs:
        sys.exit(
            f"error: no definitions found in {old_label}.\n"
            "If the schema was never committed, pass --old and --new explicitly."
        )
    if not new_defs:
        sys.exit(f"error: no definitions found in {new_label}.")

    changed = False

    # Methods, per direction. Added notifications and methods are the candidates
    # for typed wrappers; removed ones are potential breaking changes.
    for filename in METHOD_FILES:
        old_methods = methods(old_reader, filename)
        new_methods = methods(new_reader, filename)
        added = set(new_methods) - set(old_methods)
        removed = set(old_methods) - set(new_methods)
        retyped = {
            m: f"{old_methods[m]} -> {new_methods[m]}"
            for m in set(old_methods) & set(new_methods)
            if old_methods[m] != new_methods[m]
        }
        direction = filename.replace(".json", "")
        changed |= report_set(f"{direction}: added methods", added, new_methods)
        changed |= report_set(
            f"{direction}: REMOVED methods (breaking)", removed, old_methods
        )
        changed |= report_set(
            f"{direction}: params type changed (breaking)", set(retyped), retyped
        )

    # Whole types added or removed.
    changed |= report_set("added types", set(new_defs) - set(old_defs))
    changed |= report_set("REMOVED types (breaking)", set(old_defs) - set(new_defs))

    # Field-level changes on types present in both. Additions are usually new
    # optional capability; removals and shape changes break callers.
    added_fields, removed_fields, changed_fields = set(), set(), set()
    for name in sorted(set(old_defs) & set(new_defs)):
        old_props = (old_defs[name] or {}).get("properties") or {}
        new_props = (new_defs[name] or {}).get("properties") or {}
        added_fields |= {f"{name}.{f}" for f in set(new_props) - set(old_props)}
        removed_fields |= {f"{name}.{f}" for f in set(old_props) - set(new_props)}
        for field in set(old_props) & set(new_props):
            if json.dumps(old_props[field], sort_keys=True) != json.dumps(
                new_props[field], sort_keys=True
            ):
                changed_fields.add(f"{name}.{field}")
        # A field becoming required breaks existing callers.
        old_req = set((old_defs[name] or {}).get("required") or [])
        new_req = set((new_defs[name] or {}).get("required") or [])
        for field in new_req - old_req:
            if field in old_props:
                changed_fields.add(f"{name}.{field} (now required)")

    changed |= report_set("added fields", added_fields)
    changed |= report_set("REMOVED fields (breaking)", removed_fields)
    changed |= report_set("fields whose shape changed", changed_fields)

    if not changed:
        print()
        print("No schema differences found.")
        return

    print()
    print("Next steps:")
    print("  1. make generate   (fix any hand-maintained table it complains about)")
    print("  2. Re-check triStateFields against the TypeScript output; the JSON")
    print("     Schema collapses Option<Option<T>> and hides those fields.")
    print("  3. Decide which added methods/notifications deserve typed wrappers.")
    print("  4. Anything marked breaking above is worth raising with the user.")


def read_version_dir(directory):
    if not directory:
        return "unknown"
    path = Path(directory) / "VERSION"
    return path.read_text().strip() if path.exists() else "unknown"


def read_version_git(ref, directory):
    result = subprocess.run(
        ["git", "show", f"{ref}:{Path(directory).as_posix()}/VERSION"],
        capture_output=True,
        text=True,
    )
    return result.stdout.strip() if result.returncode == 0 else "unknown"


if __name__ == "__main__":
    main()
