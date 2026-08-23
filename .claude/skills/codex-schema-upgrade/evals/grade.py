#!/usr/bin/env python3
"""Grade eval runs for the codex-schema-upgrade skill.

Most of these assertions are mechanically checkable — did a dispatch case get
added, was a guard deleted, was the vendored schema touched. Checking them with a
script rather than by eye is faster, reproducible across iterations, and immune
to wishful reading of a summary that claims more than the diff shows.

The few assertions that need judgment (does the report *identify* something as a
breaking change) are graded by inspecting the summary text for the specific
tokens that would have to appear, and flagged for human confirmation.

Usage:
  grade.py <iteration-dir> <pristine-repo>
"""

import json
import re
import subprocess
import sys
from pathlib import Path

# When set, file comparisons read the baseline from this git ref instead of the
# pristine working tree, so later commits are not blamed on the runs.
BASELINE_REF = None


def baseline_bytes(pristine, rel):
    """Read a file from the pinned baseline ref, or the working tree."""
    if BASELINE_REF:
        result = subprocess.run(
            ["git", "-C", str(pristine), "show", f"{BASELINE_REF}:{rel}"],
            capture_output=True,
        )
        return result.stdout if result.returncode == 0 else None
    p = Path(pristine) / rel
    return p.read_bytes() if p.exists() else None

EVAL_DIRS = {
    1: "eval-1-regenerate-integrate",
    2: "eval-2-wrap-existing",
    3: "eval-3-stop-broken-superset",
}


def read(path):
    p = Path(path)
    return p.read_text(errors="replace") if p.exists() else ""


def repo_file(run, rel):
    return read(Path(run) / "repo" / rel)


def outputs_text(run):
    """All prose the run produced, for judgment-based assertions.

    Scans every readable text file under outputs/, not just .md and .txt: a run
    may name its report differently, and grading must not credit or penalize a
    run for filename choices.
    """
    parts = []
    out = Path(run) / "outputs"
    if out.exists():
        for f in out.rglob("*"):
            if f.is_file() and f.stat().st_size < 2_000_000:
                text = read(f)
                if text and "\x00" not in text[:1000]:
                    parts.append(text)
    return "\n".join(parts)


def file_differs(run, pristine, rel):
    """Did the run modify this file relative to the pristine repo?"""
    a = Path(run) / "repo" / rel
    b = Path(pristine) / rel
    if not a.exists() or not b.exists():
        return a.exists() != b.exists()
    return a.read_bytes() != b.read_bytes()


def dir_differs(run, pristine, reldir, exclude=()):
    """List files under reldir that differ from the pristine baseline.

    The baseline must be the repo state the runs were COPIED FROM, not current
    HEAD. If the real repo moves on while evals are in flight, comparing against
    HEAD reports the repo's own later commits as edits made by the run. Pass
    --baseline-ref to pin it.
    """
    a = Path(run) / "repo" / reldir
    b = Path(pristine) / reldir
    if not a.exists():
        return False
    changed = []
    for f in a.rglob("*"):
        if not f.is_file():
            continue
        rel = f.relative_to(a)
        if any(str(rel).startswith(e) for e in exclude):
            continue
        base = baseline_bytes(pristine, str(Path(reldir) / rel))
        if base is None or base != f.read_bytes():
            changed.append(str(rel))
    return changed


def build_passes(run):
    text = read(Path(run) / "outputs" / "build_test_output.txt")
    if not text:
        return False, "build_test_output.txt missing"
    # A run that reports failures anywhere in the go test output did not pass.
    if re.search(r"^FAIL", text, re.M) or "build failed" in text:
        return False, "FAIL present in build/test output"
    if "ok  " not in text and "PASS" not in text:
        return False, "no ok/PASS in output"
    return True, "build, vet and tests report success"


def grade_eval1(run, pristine):
    res = []
    methods_go = repo_file(run, "internal/cmd/schemagen/methods.go")
    handler_go = repo_file(run, "codex/handler.go")
    dispatch_go = repo_file(run, "codex/dispatch.go")
    gen_dir = Path(run) / "repo" / "codex" / "protocol"
    gen = "\n".join(read(f) for f in gen_dir.glob("*_gen.go")) if gen_dir.exists() else ""
    prose = outputs_text(run)

    has_entry = bool(re.search(r'"vibes/reset"\s*:', methods_go))
    res.append(("1a", has_entry,
                "resultOverrides contains a \"vibes/reset\" entry" if has_entry
                else "no \"vibes/reset\" entry found in methods.go"))

    named_right = "VibesResetResponse" in methods_go
    res.append(("1b", named_right,
                "maps to VibesResetResponse, which exists in the schema" if named_right
                else "does not map to the real VibesResetResponse type"))

    regen = "VibesChangedNotification" in gen and "thread/vibes/changed" in gen
    res.append(("1c", regen,
                "generated code contains VibesChangedNotification and the notify constant" if regen
                else "generated code lacks the new type or constant (not regenerated)"))

    cb = re.search(r"On\w*Vibes\w*\s+func\(", handler_go)
    res.append(("1d", bool(cb),
                f"Handler has callback field: {cb.group(0).strip()}" if cb
                else "no Vibes callback field in Handler"))

    disp = "Vibes" in dispatch_go and re.search(r"case protocol\.Notify\w*Vibes", dispatch_go)
    res.append(("1e", bool(disp),
                "dispatch.go has the matching case arm, so the callback fires" if disp
                else "NO dispatch case: the callback is registered but never invoked"))

    test_files = "\n".join(
        read(f) for f in (Path(run) / "repo" / "codex").glob("*_test.go")
    )
    tested = "vibes" in test_files.lower()
    res.append(("1f", tested,
                "a test exercises the new notification" if tested
                else "no test references the new notification"))

    ok, why = build_passes(run)
    res.append(("1g", ok, why))

    lower = prose.lower()
    mentions_removed = "agentnickname" in lower
    mentions_required = "startedat" in lower
    both = mentions_removed and mentions_required
    res.append(("1h", both,
                "report names both agentNickname removal and startedAt becoming required"
                if both else
                f"report mentions agentNickname={mentions_removed}, startedAt={mentions_required}"))

    touched = dir_differs(run, pristine, "internal/schemas")
    # The fixture is expected to differ; compare against the fixture instead.
    fixture = Path(pristine) / ".claude/skills/codex-schema-upgrade/evals/fixtures/future-schema"
    real_edits = []
    for name in touched or []:
        a = Path(run) / "repo" / "internal/schemas" / name
        f = fixture / name
        if not f.exists() or f.read_bytes() != a.read_bytes():
            real_edits.append(name)
    clean = not real_edits
    res.append(("1i", clean,
                "internal/schemas matches the staged fixture (not hand-edited)" if clean
                else f"vendored schema was edited: {real_edits}"))
    return res


def grade_eval2(run, pristine):
    res = []
    methods_go = repo_file(run, "codex/methods.go")
    prose = outputs_text(run)

    sig = re.search(
        r"func \(c \*Client\) ThreadSearch\(ctx context\.Context, params protocol\.ThreadSearchParams\) \(\*protocol\.ThreadSearchResponse, error\)",
        methods_go,
    )
    res.append(("2a", bool(sig),
                "ThreadSearch has the requested signature" if sig
                else "ThreadSearch missing or has a different signature"))

    uses_const = "protocol.MethodThreadSearch" in methods_go
    uses_literal = '"thread/search"' in methods_go
    res.append(("2b", uses_const and not uses_literal,
                "uses the generated MethodThreadSearch constant" if uses_const and not uses_literal
                else f"const={uses_const} literal={uses_literal}"))

    body = sig and re.search(
        r"ThreadSearch\(ctx.{0,500}?var out protocol\.ThreadSearchResponse.{0,500}?"
        r"c\.conn\.Call\(ctx, protocol\.MethodThreadSearch.{0,200}?return &out, nil",
        methods_go,
        re.S,
    )
    res.append(("2c", bool(body),
                "follows the established thin-layer shape" if body
                else "does not follow the var out / call / return &out shape"))

    schema_edits = dir_differs(run, pristine, "internal/schemas")
    proto_edits = dir_differs(run, pristine, "codex/protocol")
    untouched = not schema_edits and not proto_edits
    res.append(("2d", untouched,
                "did not re-sync or regenerate; schema and generated code untouched" if untouched
                else f"schema edits={schema_edits or 'none'}, protocol edits={proto_edits or 'none'}"))

    exp = "experimental" in (methods_go + prose).lower()
    res.append(("2e", exp,
                "notes the experimental requirement" if exp
                else "never mentions that the method requires WithExperimentalAPI"))

    ok, why = build_passes(run)
    res.append(("2f", ok, why))
    return res


def grade_eval3(run, pristine):
    res = []
    prose = outputs_text(run)
    lower = prose.lower()

    reported = "superset" in lower
    res.append(("3a", reported,
                "reports the strict-superset failure" if reported
                else "never mentions the superset invariant"))

    named = "servicename" in lower
    res.append(("3b", named,
                "names ThreadStartParams.serviceName" if named
                else "does not name the specific field"))

    exp_go = repo_file(run, "internal/cmd/schemagen/experimental.go")
    base = baseline_bytes(pristine, "internal/cmd/schemagen/experimental.go")
    pristine_exp = base.decode("utf-8", "replace") if base else ""
    guard_intact = exp_go.strip() == pristine_exp.strip()
    res.append(("3d", guard_intact,
                "the strict-superset check is untouched" if guard_intact
                else "experimental.go was modified: the guard may have been weakened"))

    schema_edits = dir_differs(run, pristine, "internal/schemas")
    fixture = Path(pristine) / ".claude/skills/codex-schema-upgrade/evals/fixtures/broken-superset"
    real_edits = []
    for name in schema_edits or []:
        a = Path(run) / "repo" / "internal/schemas" / name
        f = fixture / name
        if not f.exists() or f.read_bytes() != a.read_bytes():
            real_edits.append(name)
    res.append(("3e", not real_edits,
                "vendored schema untouched" if not real_edits
                else f"vendored schema was edited: {real_edits}"))

    # Stopping is evidenced by the guard intact, the schema intact, and generated
    # output not rewritten.
    proto_edits = dir_differs(run, pristine, "codex/protocol")
    stopped = guard_intact and not real_edits and not proto_edits
    res.append(("3c", stopped,
                "stopped and escalated without forcing a generation" if stopped
                else f"proceeded anyway (guard_intact={guard_intact}, schema_clean={not real_edits}, protocol_edits={proto_edits or 'none'})"))

    adr = "0004" in prose or "adr" in lower
    res.append(("3f", adr,
                "connects the failure to the ADR it protects" if adr
                else "does not reference the architectural decision"))

    res.append(("3g", not proto_edits,
                "codex/protocol left unchanged" if not proto_edits
                else f"generated code was rewritten despite failed validation: {proto_edits}"))
    return res


GRADERS = {1: grade_eval1, 2: grade_eval2, 3: grade_eval3}


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    iteration, pristine = sys.argv[1], sys.argv[2]

    # Pin the baseline to the commit the runs were copied from. Without this, any
    # commit landing in the real repo mid-eval is misattributed to every run.
    baseline_ref = sys.argv[3] if len(sys.argv) > 3 else None
    if baseline_ref:
        global BASELINE_REF
        BASELINE_REF = baseline_ref
        print(f"grading against baseline ref {baseline_ref}")

    for eval_id, dirname in EVAL_DIRS.items():
        meta_path = Path(iteration) / dirname / "eval_metadata.json"
        if not meta_path.exists():
            continue
        meta = json.loads(meta_path.read_text())
        texts = {a["id"]: a["text"] for a in meta.get("assertions", [])}

        for cfg in ("with_skill", "without_skill"):
            run = Path(iteration) / dirname / cfg
            if not (run / "outputs").exists():
                print(f"skip {dirname}/{cfg}: no outputs yet")
                continue
            graded = GRADERS[eval_id](str(run), pristine)
            # Keep assertion order stable for the viewer.
            order = list(texts.keys())
            graded.sort(key=lambda g: order.index(g[0]) if g[0] in order else 99)
            expectations = [
                {"text": f"{aid}: {texts.get(aid, aid)}", "passed": bool(ok), "evidence": ev}
                for aid, ok, ev in graded
            ]
            passed = sum(1 for e in expectations if e["passed"])
            result = {
                "eval_id": eval_id,
                "eval_name": meta.get("eval_name"),
                "configuration": cfg,
                "expectations": expectations,
                "passed_count": passed,
                "total_count": len(expectations),
                "pass_rate": round(passed / len(expectations), 3) if expectations else 0,
            }
            (run / "grading.json").write_text(json.dumps(result, indent=2))
            print(f"{dirname}/{cfg}: {passed}/{len(expectations)}")
            for e in expectations:
                if not e["passed"]:
                    print(f"    FAIL {e['text'][:70]} -- {e['evidence'][:80]}")


if __name__ == "__main__":
    main()
