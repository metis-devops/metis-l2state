#!/usr/bin/env python3
"""Fresh-process paired migration measurements; synthetic fixtures only."""
import argparse
import hashlib
import json
import itertools
import math
import os
from pathlib import Path
import platform
import re
import subprocess
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]


def run(command, cwd=ROOT, env=None):
    subprocess.run(command, cwd=cwd, env=env, check=True)


def measure(command, directory):
    directory.mkdir()
    logs = directory.parent / (directory.name + ".log")
    result = {}
    with logs.open("wb") as stderr, (directory.parent / (directory.name + ".json")).open("wb") as stdout:
        started = time.monotonic()
        process = subprocess.Popen(command, stdout=stdout, stderr=stderr)

        def wait():
            _, status, usage = os.wait4(process.pid, 0)
            process.returncode = os.waitstatus_to_exitcode(status)
            result.update(seconds=time.monotonic() - started,
                          max_rss_bytes=usage.ru_maxrss * (1 if platform.system() == "Darwin" else 1024),
                          user_seconds=usage.ru_utime, system_seconds=usage.ru_stime)

        waiter = threading.Thread(target=wait)
        waiter.start()
        peak = 0
        while waiter.is_alive():
            total = 0
            for path in directory.rglob("*"):
                try:
                    if path.is_file():
                        total += path.stat().st_size
                except FileNotFoundError:
                    pass  # Atomic publication can move the staged directory.
            peak = max(peak, total)
            waiter.join(0.02)
        if process.returncode:
            raise RuntimeError(logs.read_text())
    result["sampled_peak_output_and_scratch_bytes"] = peak
    result["artifact_bytes"] = sum(p.stat().st_size for p in directory.rglob("*") if p.is_file())
    return result


def source_identity(directory):
    records = []
    for path in sorted(directory.rglob("*")):
        if path.is_file():
            with path.open("rb") as stream:
                records.append(dict(path=str(path.relative_to(directory)), size=path.stat().st_size,
                                    sha256=hashlib.file_digest(stream, "sha256").hexdigest()))
    encoded = json.dumps(records, sort_keys=True).encode()
    return dict(sha256=hashlib.sha256(encoded).hexdigest(), files=records)


def check_observations(observations, comparison):
    if len({item["final_root"] for item in observations}) != 1:
        raise RuntimeError("baseline/current transformed roots differ")
    statefiles = [item for item in observations if "summary" in item]
    if len({json.dumps(item["summary"], sort_keys=True) for item in statefiles}) > 1:
        raise RuntimeError("statefile summaries differ")
    groups = {}
    for item in statefiles:
        group = item.get("encoding", item["implementation"]) if comparison == "summary" else "all"
        groups.setdefault(group, set()).add(item["artifact_sha256"])
    if any(len(hashes) != 1 for hashes in groups.values()):
        raise RuntimeError("statefile output bytes differ within comparison group")


def validate_allocation_sample(raw, benchmark):
    pattern = r"^" + re.escape(benchmark) + r"(?:-\d+)?\s+1\s+[\d.]+ ns/op\s+[\d.]+ B/op\s+[\d.]+ allocs/op"
    if len(re.findall(pattern, raw, re.M)) != 1 or "PASS" not in raw:
        raise RuntimeError("expected exactly one successful allocation sample: " + raw)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    baselines = parser.add_mutually_exclusive_group()
    baselines.add_argument("--baseline-root", type=Path, help="legacy database-output checkout")
    baselines.add_argument("--baseline-binary", type=Path, help="saved statefile-output l2state binary")
    parser.add_argument("--baseline-events", action="store_true", help="baseline also requires --ovm-events")
    parser.add_argument("--baseline-test-binary", type=Path, help="saved migration go test -c binary; pairs allocation measurements")
    parser.add_argument("--baseline-verifier", type=Path, help="saved verifier matching the baseline encoding")
    parser.add_argument("--compare", choices=["bytes", "summary"], default="bytes",
                        help="summary explicitly permits different encodings; same-implementation bytes must still match")
    parser.add_argument("--command", choices=["migrate", "dump"], default="migrate")
    parser.add_argument("--code-cache", choices=["memory", "none", "both"], help="current code cache modes; baseline uses its original default")
    parser.add_argument("--compression", choices=["none", "zstd"], default="zstd")
    parser.add_argument("--accounts", type=int, default=10000)
    parser.add_argument("--codes", type=int, default=0,
                        help="give this many extra accounts distinct 10-byte contract code")
    parser.add_argument("--code-bytes", type=int, default=10, help="bytes per distinct fixture code, 10 through 1 MiB")
    parser.add_argument("--storage-contracts", type=int, default=0)
    parser.add_argument("--storage-slots", type=int, default=0)
    parser.add_argument("--storage-alloc", action="store_true", help="replace code and edit populated storage after conversion")
    parser.add_argument("--noop-alloc", action="store_true",
                        help="exercise overlay collection with an unchanged existing account")
    parser.add_argument("--pairs", type=int, default=6)
    parser.add_argument("--workers", type=int, default=2)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    if not 10 <= args.code_bytes <= 1 << 20:
        parser.error("invalid code byte size")
    if args.accounts < 0 or not 0 <= args.codes <= args.accounts or args.pairs < 1 or not 2 <= args.workers <= 16:
        parser.error("invalid measurement parameters")
    if not 0 <= args.storage_contracts <= args.accounts or args.storage_slots < 0 or ((args.storage_contracts == 0) != (args.storage_slots == 0)):
        parser.error("invalid storage fixture dimensions")
    if args.storage_alloc and (not args.storage_contracts or args.noop_alloc):
        parser.error("storage alloc requires storage and cannot combine with noop alloc")
    if args.baseline_test_binary and not args.baseline_binary:
        parser.error("--baseline-test-binary requires --baseline-binary")
    if args.baseline_verifier and not args.baseline_binary:
        parser.error("--baseline-verifier requires --baseline-binary")
    if args.compare == "summary" and not (args.baseline_binary and args.baseline_verifier):
        parser.error("--compare summary requires --baseline-binary and --baseline-verifier")
    if args.command == "dump" and (args.noop_alloc or args.storage_alloc or args.baseline_root):
        parser.error("dump cannot use alloc or a legacy migration baseline")
    args.out = args.out.resolve()
    args.out.mkdir(parents=True, exist_ok=False)
    with tempfile.TemporaryDirectory(prefix="l2state-benchmark-") as temporary:
        work = Path(temporary)
        current = work / "current"
        run(["go", "build", "-trimpath", "-o", str(current), "./cmd/l2state"])
        verifier = work / "verifyfile"
        run(["go", "build", "-trimpath", "-o", str(verifier), "./testdata/verifyfile"])
        fixture = work / "fixture"
        env = dict(os.environ, L2STATE_BENCH_FIXTURE=str(fixture), L2STATE_BENCH_ACCOUNTS=str(args.accounts),
                   L2STATE_BENCH_CODES=str(args.codes), L2STATE_BENCH_CODE_BYTES=str(args.code_bytes), L2STATE_BENCH_STORAGE_CONTRACTS=str(args.storage_contracts),
                   L2STATE_BENCH_STORAGE_SLOTS=str(args.storage_slots))
        run(["go", "test", "-run", "^TestBenchmarkFixture$", "-count=1", "-timeout=10m"], ROOT / "testdata/legacyintegration", env)
        source_before = source_identity(fixture / "source")
        alloc = work / "alloc.json"
        if args.noop_alloc:
            alloc.write_text('{"0x0000000000000000000000000000000000000103":{}}\n')
        if args.storage_alloc:
            alloc.write_text(json.dumps({"0x0000000000000000000000000000000000001000": {
                "balance": "0x0", "nonce": "0x0", "code": "0x", "storage": {"0x00": "0x00", "0x01": "0x99"}}}) + "\n")
        programs = {"current": current}
        if args.code_cache:
            modes = ["memory", "none"] if args.code_cache == "both" else [args.code_cache]
            programs = {"current-" + mode: current for mode in modes}
        if args.baseline_root:
            baseline = work / "baseline"
            run(["go", "build", "-trimpath", "-o", str(baseline), "./cmd/l2state"], args.baseline_root.resolve())
            programs["baseline"] = baseline
        if args.baseline_binary:
            programs["baseline"] = args.baseline_binary.resolve()
        observations = []
        for pair in range(args.pairs):
            order = list(itertools.permutations(programs))[pair % math.factorial(len(programs))]
            for name in order:
                directory = args.out / f"{pair:02d}-{name}"
                target = directory / "artifact"
                command = [str(programs[name]), args.command, "--source-chaindata", str(fixture / "source"), "--out", str(target),
                           "--workers", str(args.workers), "--cache-mb", "64", "--handles", "64", "--quiet"]
                if name.startswith("current-"):
                    command += ["--code-cache", name.removeprefix("current-")]
                if args.command == "migrate":
                    command += ["--wrapped-ether-code", str(ROOT / "internal/migration/testdata/wrapped.hex")]
                if args.command == "migrate" and (name != "baseline" or args.baseline_events):
                    command += ["--ovm-events", str(fixture / "events.json")]
                if args.noop_alloc or args.storage_alloc:
                    command += ["--ovm-genesis-alloc", str(alloc)]
                legacy = name == "baseline" and args.baseline_root is not None
                if legacy:
                    command += ["--migrate-ovm-eth", "--db-engine", "pebble", "--scheme", "hash"]
                else:
                    command += ["--compression", args.compression]
                measured = measure(command, directory)
                report_path = target / "verification.json" if legacy else directory.parent / (directory.name + ".json")
                report = json.loads(report_path.read_text())
                measured["final_root"] = report["target_state"]["state_root"] if legacy else report["summary"]["FinalStateRoot"]
                if not legacy:
                    measured["summary"] = report["summary"]
                    if args.codes and not args.storage_alloc and report["summary"]["Counts"]["Codes"] != args.codes + (1 if args.command == "dump" else 2):
                        raise RuntimeError("distinct-code fixture did not preserve every contract code")
                    if args.storage_contracts:
                        expected_slots = (args.accounts + 5 if args.command == "dump" else 3) + args.storage_contracts * args.storage_slots - int(args.storage_alloc)
                        if report["summary"]["Counts"]["StorageSlots"] != expected_slots:
                            raise RuntimeError("storage fixture inventory differs from requested workload")
                    with target.open("rb") as artifact:
                        measured["artifact_sha256"] = hashlib.file_digest(artifact, "sha256").hexdigest()
                measured.update(pair=pair, implementation=name, encoding="baseline" if name == "baseline" else "current")
                observations.append(measured)
                print(json.dumps(measured), flush=True)
        check_observations(observations, args.compare)
        verifiers = {name: verifier for name in programs}
        if args.baseline_verifier:
            verifiers["baseline"] = args.baseline_verifier.resolve()
        # Validate every artifact only after all timed CLI runs, so readback does
        # not enter migration timing or warm caches between paired invocations.
        for item in observations:
            if "summary" not in item:
                continue
            target = args.out / f"{item['pair']:02d}-{item['implementation']}" / "artifact"
            verified = json.loads(subprocess.check_output([str(verifiers[item["implementation"]]), str(target)], cwd=ROOT))
            if verified != item["summary"]:
                raise RuntimeError("independent readback summary differs from migration")
            item["independently_verified"] = True
        source_after = source_identity(fixture / "source")
        if source_before != source_after:
            raise RuntimeError("source database changed")
        result = dict(command=args.command, source_before=source_before, source_after=source_after, accounts=args.accounts, distinct_codes=args.codes, code_bytes=args.code_bytes, noop_alloc=args.noop_alloc, storage_contracts=args.storage_contracts, storage_slots=args.storage_slots, storage_alloc=args.storage_alloc,
                      alloc_sha256=hashlib.sha256(alloc.read_bytes()).hexdigest() if args.noop_alloc or args.storage_alloc else None,
                      workers=args.workers, compression=args.compression, platform=platform.platform(), observations=observations,
                      binary_sha256={name: hashlib.sha256(path.read_bytes()).hexdigest() for name, path in programs.items()},
                      comparison=args.compare, code_cache=args.code_cache,
                      event_json_sha256=hashlib.sha256((fixture / "events.json").read_bytes()).hexdigest(),
                      verifier_sha256={name: hashlib.sha256(path.read_bytes()).hexdigest() for name, path in verifiers.items()},
                      limitations="Synthetic, same-source, fresh-process comparison. " +
                      ("Legacy output format differs. " if args.baseline_root else
                       "Cross-provenance/encoding summaries match; bytes match within each implementation. " if args.compare == "summary" else
                       "Statefile output SHA-256 must match. ") +
                      "Disk peak sampled every 20ms; not an exact peak. No production throughput claim.")
        (args.out / "results.json").write_text(json.dumps(result, indent=2) + "\n")
        result["allocation_fixture"] = dict(command=args.command, contract_storage_slots=10000, workers=2,
                                            cache_mb=16, compression="zstd", benchtime="1x")
        # Allocation fixture differs: one contract with 10,000 storage slots.
        # Build once, then execute each sample in its own alternating process.
        current_test = work / "current.test"
        run(["go", "test", "-c", "-o", str(current_test), "./internal/migration"])
        test_programs = {name: current_test for name in programs if name != "baseline"}
        if args.baseline_test_binary:
            test_programs["baseline"] = args.baseline_test_binary.resolve()
        result["test_binary_sha256"] = {name: hashlib.sha256(path.read_bytes()).hexdigest()
                                        for name, path in test_programs.items()}
        with (args.out / "allocations.txt").open("w") as output:
            for pair in range(args.pairs):
                order = list(itertools.permutations(test_programs))[pair % math.factorial(len(test_programs))]
                for name in order:
                    output.write(f"pair={pair} implementation={name}\n")
                    output.flush()
                    benchmark = "BenchmarkDump" if args.command == "dump" else "BenchmarkMigrate"
                    raw = subprocess.check_output([str(test_programs[name]), "-test.run", "^$", "-test.bench", "^" + benchmark + "$",
                                                   "-test.benchtime=1x", "-test.benchmem", "-test.count=1"], cwd=ROOT, text=True,
                                                   env=dict(os.environ, L2STATE_BENCH_CODE_CACHE=name.removeprefix("current-") if name.startswith("current-") else ""))
                    validate_allocation_sample(raw, benchmark)
                    output.write(raw)
        (args.out / "results.json").write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
