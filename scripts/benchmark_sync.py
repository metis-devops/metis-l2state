#!/usr/bin/env python3
"""Fresh-process, alternating HTTP event-sync samples; verification is outside Go timing."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import tempfile
import time


def run_suite(baseline, current, pairs):
    samples, raw_outputs = [], []
    for density in ("empty", "sparse", "dense"):
        digest = None
        for pair in range(pairs):
            order = ("baseline", "current") if pair % 2 == 0 else ("current", "baseline")
            for variant in order:
                binary = baseline if variant == "baseline" else current
                command = [str(binary), "-test.run=^$", "-test.bench=^BenchmarkSyncBoundary$/" + density + "$",
                           "-test.benchtime=1x", "-test.benchmem", "-test.count=1"]
                # A file avoids pipe backpressure and retains the complete benchmark output.
                with tempfile.TemporaryFile(mode="w+") as stream:
                    start = time.monotonic()
                    proc = subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT)
                    _, status, usage = os.wait4(proc.pid, 0)
                    seconds = time.monotonic() - start
                    proc.returncode = os.waitstatus_to_exitcode(status)
                    stream.seek(0)
                    raw = stream.read()
                if proc.returncode:
                    raise RuntimeError(raw)
                rows = re.findall(r"^BenchmarkSyncBoundary/\S+\s+1\s+(.+)$", raw, re.M)
                identities = set(re.findall(r"CHECKPOINT sha256=([0-9a-f]{64})", raw))
                if len(rows) != 1 or len(identities) != 1 or "PASS" not in raw:
                    raise RuntimeError("incomplete sync benchmark: " + raw)
                tokens = rows[0].split()
                metrics = {tokens[i + 1]: float(tokens[i]) for i in range(0, len(tokens), 2)}
                actual = identities.pop()
                if digest is not None and digest != actual:
                    raise RuntimeError("checkpoint parity failed")
                digest = actual
                samples.append(dict(density=density, pair=pair, variant=variant, metrics=metrics,
                                    checkpoint_sha256=digest, process_seconds=seconds,
                                    process_user_seconds=usage.ru_utime, process_system_seconds=usage.ru_stime,
                                    process_peak_rss_bytes=usage.ru_maxrss * (1 if platform.system() == "Darwin" else 1024),
                                    command=command))
                raw_outputs.append(f"\n# Sync {density} pair={pair} variant={variant}\n" + raw)
    return dict(blocks=2000, batch_size=1000, pairs=pairs, transport="localhost HTTP, no injected delay",
                verification="expected checkpoint bytes and strict ReadFile outside Go benchmark timer",
                binaries={name: hashlib.sha256(path.read_bytes()).hexdigest()
                          for name, path in (("baseline", baseline), ("current", current))}, samples=samples), "".join(raw_outputs)


def write_results(directory, payload, raw, baseline_ref, baseline_identity=None):
    root = Path(__file__).resolve().parents[1]
    sources = [root / "go.mod", root / "go.sum", Path(__file__).resolve()]
    for package in ("ovmevents", "ethjson", "strictio"):
        sources.extend((root / "internal" / package).glob("*.go"))
    payload.update(baseline_ref=baseline_ref, baseline_identity=baseline_identity,
                   source_sha256={str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest()
                                  for p in sorted(sources)},
                   environment={"platform": platform.platform(), "uname": list(platform.uname()),
                                "go_version": subprocess.check_output(["go", "version"], text=True).strip(),
                                "variables": {key: os.environ.get(key) for key in
                                              ("GOGC", "GOMEMLIMIT", "GOMAXPROCS", "GOEXPERIMENT", "GOFLAGS")}})
    parity = {"verification": payload["verification"], "samples": len(payload["samples"]),
              "checkpoint_sha256": {s["density"]: s["checkpoint_sha256"] for s in payload["samples"]}}
    (directory / "paired.json").write_text(json.dumps(payload, indent=2) + "\n")
    (directory / "allocations.txt").write_text(raw)
    (directory / "parity.json").write_text(json.dumps(parity, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--current", type=Path, required=True)
    parser.add_argument("--pairs", type=int, default=6)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--baseline-ref", required=True)
    parser.add_argument("--baseline-identity", type=Path)
    args = parser.parse_args()
    if args.pairs < 1:
        parser.error("pairs must be positive")
    args.out.mkdir(parents=True, exist_ok=False)
    payload, raw = run_suite(args.baseline.resolve(), args.current.resolve(), args.pairs)
    identity = json.loads(args.baseline_identity.read_text()) if args.baseline_identity else None
    write_results(args.out, payload, raw, args.baseline_ref, identity)


if __name__ == "__main__":
    main()
