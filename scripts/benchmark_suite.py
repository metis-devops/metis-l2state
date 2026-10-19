#!/usr/bin/env python3
"""Run and consolidate the complete local paired migration and dump benchmark suite."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import time

from benchmark import ROOT, run


def source_manifest():
    paths = set(ROOT.rglob("*.go")) | set(ROOT.rglob("go.mod")) | set(ROOT.rglob("go.sum"))
    paths |= set((ROOT / "scripts").glob("*.py"))
    return {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in sorted(paths) if ".git" not in p.parts}


def measure_index(binary, size, mode, implementation, pair, output):
    pattern = f"^BenchmarkCodeHashIndex$/{size}$/{mode}$/{implementation}$"
    command = [str(binary), "-test.run=^$", "-test.bench=" + pattern,
               "-test.benchtime=1x", "-test.benchmem", "-test.count=1"]
    with output.open("w+") as stream:
        started = time.monotonic()
        process = subprocess.Popen(command, cwd=ROOT, stdout=stream, stderr=subprocess.STDOUT)
        _, status, usage = os.wait4(process.pid, 0)
        seconds = time.monotonic() - started
        process.returncode = os.waitstatus_to_exitcode(status)
        stream.seek(0)
        raw = stream.read()
    if process.returncode:
        raise RuntimeError(raw)
    matches = re.findall(r"^Benchmark\S+\s+1\s+([\d.]+) ns/op\s+([\d.]+) B/op\s+([\d.]+) allocs/op", raw, re.M)
    if len(matches) != 1 or "PASS" not in raw:
        raise RuntimeError("expected exactly one successful index sample: " + raw)
    ns, allocated, allocations = map(float, matches[0])
    return dict(pair=pair, implementation=implementation, distinct_hashes=size, mode=mode,
                ns_per_op=ns, bytes_per_op=allocated, allocs_per_op=allocations,
                process_seconds=seconds, process_user_seconds=usage.ru_utime,
                process_system_seconds=usage.ru_stime,
                process_peak_rss_bytes=usage.ru_maxrss * (1 if platform.system() == "Darwin" else 1024)), raw


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline-binary", type=Path, required=True)
    parser.add_argument("--baseline-test-binary", type=Path, required=True)
    parser.add_argument("--baseline-verifier", type=Path, required=True)
    parser.add_argument("--baseline-ref", required=True)
    parser.add_argument("--baseline-identity", type=Path,
                        help="source manifest and reconstruction data for an uncommitted baseline")
    parser.add_argument("--query-analysis", type=Path,
                        help="same-campaign dump profiles, candidate samples and reconstruction data")
    parser.add_argument("--compare", choices=["bytes", "summary"], default="bytes")
    parser.add_argument("--baseline-index", choices=["btree", "ordered"], default="ordered")
    parser.add_argument("--pairs", type=int, default=6)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    if args.pairs < 1:
        parser.error("pairs must be positive")
    args.out = args.out.resolve()
    args.out.mkdir(parents=True, exist_ok=False)
    os.environ["GOFLAGS"] = "-buildvcs=false"
    os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
    for key in ["L2STATE_BENCH_CODE_BYTES", "L2STATE_BENCH_CODE_CACHE", "L2STATE_BENCH_CODES", "L2STATE_BENCH_STORAGE_CONTRACTS", "L2STATE_BENCH_STORAGE_SLOTS"]:
        os.environ.pop(key, None)
    manifest = source_manifest()
    configurations = [
        ("1k-bytecode-w8-zstd", 1024, 1024, 8, "zstd"),
        ("1k-bytecode-w8-none", 1024, 1024, 8, "none"),
        ("10k-w2-zstd", 10000, 0, 2, "zstd"),
        ("10k-w8-zstd", 10000, 0, 8, "zstd"),
        ("10k-w8-none", 10000, 0, 8, "none"),
        ("100k-w8-zstd", 100000, 0, 8, "zstd"),
        ("10k-codes-w8-zstd", 10000, 10000, 8, "zstd"),
        ("10k-codes-w8-none", 10000, 10000, 8, "none"),
        ("100k-codes-w8-zstd", 100000, 100000, 8, "zstd"),
        ("100k-codes-w8-none", 100000, 100000, 8, "none"),
        ("10k-codes-w8-zstd-alloc", 10000, 10000, 8, "zstd"),
        ("10k-codes-w8-none-alloc", 10000, 10000, 8, "none"),
        ("100k-codes-w8-zstd-alloc", 100000, 100000, 8, "zstd"),
        ("100k-codes-w8-none-alloc", 100000, 100000, 8, "none"),
    ]
    configurations += [
        ("32x1025-storage-w8-zstd", 32, 0, 8, "zstd"),
        ("1m-storage-w8-zstd", 1, 0, 8, "zstd"),
        ("1m-storage-w8-none", 1, 0, 8, "none"),
        ("1m-storage-w8-zstd-overlay", 1, 0, 8, "zstd"),
    ]
    # Retain the complete migration suite, adding raw dump counterparts for
    # every workload without an overlay. Both commands use the same fixtures.
    dump_configurations = [("dump-" + name, accounts, codes, workers, codec)
                           for name, accounts, codes, workers, codec in configurations
                           if not name.endswith(("-alloc", "-overlay"))]
    configurations += dump_configurations
    results = {}
    allocation_text = []
    baseline_args = ["--baseline-binary", str(args.baseline_binary.resolve()),
                     "--baseline-test-binary", str(args.baseline_test_binary.resolve()),
                     "--baseline-verifier", str(args.baseline_verifier.resolve()), "--baseline-events", "--compare", args.compare, "--code-cache", "both"]
    for name, accounts, codes, workers, codec in configurations:
        print("CLI phase: " + name, flush=True)
        directory = args.out / name
        overlay_args = ["--noop-alloc"] if name.endswith("-alloc") else []
        if "bytecode" in name:
            overlay_args += ["--code-bytes", "65536"]
        if name.startswith("dump-"):
            overlay_args += ["--command", "dump"]
        if "storage" in name:
            contracts, slots = (32, 1025) if "32x1025" in name else (1, 1000000)
            overlay_args += ["--storage-contracts", str(contracts), "--storage-slots", str(slots)]
            if name.endswith("-overlay"):
                overlay_args += ["--storage-alloc"]
        with (args.out / (name + ".log")).open("w") as log:
            subprocess.run([sys.executable, str(ROOT / "scripts/benchmark.py"), *baseline_args, *overlay_args,
                            "--accounts", str(accounts), "--codes", str(codes), "--workers", str(workers),
                            "--compression", codec, "--pairs", str(args.pairs), "--out", str(directory)],
                           cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, check=True)
        results[name] = json.loads((directory / "results.json").read_text())
        allocation_text.append(f"\n# CLI suite phase: {name}\n" + (directory / "allocations.txt").read_text())
    current, verifier, test_binary = [args.out / name for name in ["l2state", "verifyfile", "migration.test"]]
    run(["go", "build", "-trimpath", "-o", str(current), "./cmd/l2state"])
    run(["go", "build", "-trimpath", "-o", str(verifier), "./testdata/verifyfile"])
    run(["go", "test", "-c", "-o", str(test_binary), "./internal/migration"])
    print("Independent overlay/retain parity", flush=True)
    run([sys.executable, str(ROOT / "scripts/benchmark_parity.py"),
         "--baseline-binary", str(args.baseline_binary.resolve()), "--current-binary", str(current),
         "--baseline-verifier", str(args.baseline_verifier.resolve()), "--current-verifier", str(verifier),
         "--baseline-events", "--compare", args.compare, "--code-cache", "both", "--out", str(args.out / "parity")])
    samples = []
    for size in [10000, 100000, 1000000]:
        for mode in ["shared", "overlap", "repeated"]:
            print(f"Index phase: {size}/{mode}", flush=True)
            for pair in range(args.pairs):
                order = ["baseline", "current"] if pair % 2 == 0 else ["current", "baseline"]
                for implementation in order:
                    algorithm = args.baseline_index if implementation == "baseline" else "ordered"
                    sample, raw = measure_index(args.baseline_test_binary.resolve() if implementation == "baseline" else test_binary, size, mode, algorithm, pair,
                                                args.out / f"index-{size}-{mode}-{pair}-{implementation}.txt")
                    sample.update(implementation=implementation, algorithm=algorithm)
                    samples.append(sample)
                    allocation_text.append(f"\n# Index {size}/{mode} pair={pair} implementation={implementation}\n" + raw)
    if source_manifest() != manifest:
        raise RuntimeError("source changed during benchmark suite")
    payload = dict(baseline_ref=args.baseline_ref,
                   baseline_identity=json.loads(args.baseline_identity.read_text()) if args.baseline_identity else None,
                   query_analysis=json.loads(args.query_analysis.read_text()) if args.query_analysis else None,
                   source_sha256=manifest,
                   platform=platform.platform(), go_version=subprocess.check_output(["go", "version"], text=True).strip(),
                   pairs=args.pairs, comparison=args.compare, baseline_index=args.baseline_index, configurations=results, code_index=samples,
                   index_test_binary_sha256={"current": hashlib.sha256(test_binary.read_bytes()).hexdigest(), "baseline": hashlib.sha256(args.baseline_test_binary.read_bytes()).hexdigest()})
    (args.out / "paired.json").write_text(json.dumps(payload, indent=2) + "\n")
    (args.out / "allocations.txt").write_text("".join(allocation_text))
    (args.out / "parity.json").write_bytes((args.out / "parity/parity.json").read_bytes())
    print("Suite consolidated in " + str(args.out), flush=True)


if __name__ == "__main__":
    main()
