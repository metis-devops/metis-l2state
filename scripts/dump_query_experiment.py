#!/usr/bin/env python3
"""Profile and compare dump query candidates on shared stopped legacy fixtures."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess

from benchmark import ROOT, measure, source_identity


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def fixture(directory, accounts, codes):
    target = directory / f"fixture-{accounts}-{codes}"
    if not target.exists():
        env = dict(os.environ, L2STATE_BENCH_FIXTURE=str(target), L2STATE_BENCH_ACCOUNTS=str(accounts),
                   L2STATE_BENCH_CODES=str(codes), L2STATE_BENCH_STORAGE_CONTRACTS="0", L2STATE_BENCH_STORAGE_SLOTS="0")
        subprocess.run(["go", "test", "-run=^TestBenchmarkFixture$", "-count=1", "-timeout=10m"],
                       cwd=ROOT / "testdata/legacyintegration", env=env, check=True)
    return target / "source"


def verify(binary, artifact):
    return json.loads(subprocess.check_output([str(binary), str(artifact)], text=True))


def profiles(args, directory, binaries):
    records = []
    for name, codes, codec in [("shared-zstd", 0, "zstd"), ("codes-zstd", 100000, "zstd"), ("codes-none", 100000, "none")]:
        source = fixture(directory, 100000, codes)
        before = source_identity(source)
        for implementation, binary_dir in binaries.items():
            for mode in ["cpu", "locks", "alloc"]:
                print(f"Profile: {name}/{implementation}/{mode}", flush=True)
                run_dir = directory / f"profile-{name}-{implementation}-{mode}"
                run_dir.mkdir()
                program = binary_dir / "dumpprofile"
                artifact = run_dir / "artifact"
                raw = subprocess.check_output([str(program), "-source", str(source), "-out", str(artifact),
                                               "-profiles", str(run_dir / "profiles"), "-mode", mode,
                                               "-compression", codec], text=True)
                stats = json.loads(raw)
                summary = verify(binary_dir / "verifyfile", artifact)
                if summary != stats["Result"]["summary"]:
                    raise RuntimeError("profile artifact summary mismatch")
                payload = {}
                for path in sorted((run_dir / "profiles").glob("*.pprof")):
                    text = subprocess.check_output(["go", "tool", "pprof", "-top", "-nodecount=40", str(program), str(path)], text=True)
                    cumulative = subprocess.check_output(["go", "tool", "pprof", "-top", "-cum", "-nodecount=40", str(program), str(path)], text=True)
                    payload[path.name] = dict(base64=base64.b64encode(path.read_bytes()).decode(), sha256=digest(path), top=text, cumulative=cumulative)
                records.append(dict(configuration=name, implementation=implementation, mode=mode, stats=stats,
                                    executable_sha256=digest(program), artifact_sha256=digest(artifact),
                                    summary=summary, independently_verified=True, profiles=payload))
                artifact.unlink()
        after = source_identity(source)
        if before != after:
            raise RuntimeError("profile changed source")
        for item in records:
            if item["configuration"] == name:
                item.update(source_before=before, source_after=after)
    return dict(kind="profiles", allocation_sample_bytes=4096, mutex_fraction=1, block_rate_ns=1,
                note="Instrumented fresh processes; never timing evidence. Post-Dump GC flushes alloc samples outside MemStats delta.", observations=records)


def compare(args, directory, binaries):
    observations = []
    scenarios = [("shared-w8-zstd", 0, 8, "zstd"), ("codes-w8-zstd", 100000, 8, "zstd"),
                 ("codes-w8-none", 100000, 8, "none"), ("shared-w2-zstd", 0, 2, "zstd"),
                 ("shared-w16-zstd", 0, 16, "zstd")]
    for name, codes, workers, codec in scenarios:
        print("Compare: " + name, flush=True)
        source = fixture(directory, 100000, codes)
        before = source_identity(source)
        names = list(binaries)
        samples = []
        for pair in range(args.rounds):
            offset = pair % len(names)
            order = names[offset:] + names[:offset]
            for implementation in order:
                binary_dir = binaries[implementation]
                target = directory / f"compare-{name}-{pair}-{implementation}"
                command = [str(binary_dir / "l2state"), "dump", "--source-chaindata", str(source),
                           "--out", str(target / "artifact"), "--workers", str(workers),
                           "--cache-mb", "64", "--handles", "64", "--compression", codec, "--quiet"]
                item = measure(command, target)
                summary = json.loads(target.with_suffix(".json").read_text())["summary"]
                item.update(configuration=name, implementation=implementation, pair=pair, workers=workers,
                            summary=summary, artifact_sha256=digest(target / "artifact"))
                samples.append((item, target / "artifact", binary_dir / "verifyfile"))
        # Readback and hashing sources are outside all timed samples.
        for item, artifact, verifier in samples:
            if verify(verifier, artifact) != item["summary"]:
                raise RuntimeError("timed artifact summary mismatch")
            item["independently_verified"] = True
            artifact.unlink()
        if len({v[0]["artifact_sha256"] for v in samples}) != 1 or len({json.dumps(v[0]["summary"], sort_keys=True) for v in samples}) != 1:
            raise RuntimeError("candidate bytes or summaries differ")
        after = source_identity(source)
        if before != after:
            raise RuntimeError("comparison changed source")
        observations.append(dict(configuration=name, source_before=before, source_after=after,
                                 observations=[v[0] for v in samples]))
    return dict(kind="comparison", rounds=args.rounds, ordering="rotate candidate order each round; no profiles during timing",
                binary_sha256={k: digest(v / "l2state") for k, v in binaries.items()}, configurations=observations)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["profile", "compare"])
    parser.add_argument("--binary", action="append", required=True, help="name=/absolute/binary-directory")
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--result", type=Path, required=True)
    parser.add_argument("--rounds", type=int, default=8)
    args = parser.parse_args()
    if args.rounds < 1 or args.result.exists():
        parser.error("positive rounds and a new result file are required")
    binaries = {}
    for value in args.binary:
        name, path = value.split("=", 1)
        if not name.isalnum() or name in binaries:
            parser.error("unique alphanumeric candidate names required")
        binaries[name] = Path(path).resolve()
    args.work = args.work.resolve()
    args.work.mkdir(parents=True, exist_ok=True)
    result = profiles(args, args.work, binaries) if args.mode == "profile" else compare(args, args.work, binaries)
    args.result.write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
