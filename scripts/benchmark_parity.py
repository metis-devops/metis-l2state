#!/usr/bin/env python3
"""Reproduce overlay/retain parity outside migration benchmark timing."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile

from benchmark import ROOT, check_observations, run, source_identity

SCENARIOS = {
    "empty": {},
    "noop": {"0x0000000000000000000000000000000000000103": {},
             "0x0000000000000000000000000000000000000098": {"storage": {}}},
    "split": {"0x0000000000000000000000000000000000000102": {"code": "0x"},
              "0x0000000000000000000000000000000000000103": {"code": "0x60026000"},
              "0x0000000000000000000000000000000000000099": {"code": "0x60026000"},
              "0x0000000000000000000000000000000000000098": {"balance": "0x0", "storage": {"0x01": "0x00"}}},
    "retain-overlay": {"0x0000000000000000000000000000000000000101": {"balance": "0x0", "nonce": "0x0", "code": "0x"},
                       "0x0000000000000000000000000000000000000103": {"code": "0x600300", "storage": {"0x01": "0x23"}}},
    "retain-only": None,
}
RETAIN = "0x0000000000000000000000000000000000000103"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["baseline-binary", "current-binary", "baseline-verifier", "current-verifier"]:
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--baseline-events", action="store_true")
    parser.add_argument("--compare", choices=["bytes", "summary"], default="bytes")
    parser.add_argument("--code-cache", choices=["both"], help="verify current memory/none bytes together")
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=False)
    programs = {"baseline": args.baseline_binary.resolve(), "current": args.current_binary.resolve()}
    verifiers = {"baseline": args.baseline_verifier.resolve(), "current": args.current_verifier.resolve()}
    if args.code_cache == "both":
        programs = {"baseline": args.baseline_binary.resolve(), "current-memory": args.current_binary.resolve(), "current-none": args.current_binary.resolve()}
        verifiers = {name: args.baseline_verifier.resolve() if name == "baseline" else args.current_verifier.resolve() for name in programs}
    observations = []
    with tempfile.TemporaryDirectory(prefix="l2state-parity-") as tmp:
        work = Path(tmp)
        fixture = work / "fixture"
        env = dict(os.environ, L2STATE_BENCH_FIXTURE=str(fixture), L2STATE_BENCH_ACCOUNTS="10000")
        run(["go", "test", "-run", "^TestBenchmarkFixture$", "-count=1"], ROOT / "testdata/legacyintegration", env)
        source_before = source_identity(fixture / "source")
        events_hash = hashlib.sha256((fixture / "events.json").read_bytes()).hexdigest()
        retain = work / "retain.txt"
        retain.write_text(RETAIN + "\n")
        for scenario, alloc in SCENARIOS.items():
            alloc_path = work / (scenario + ".json")
            if alloc is not None:
                alloc_path.write_text(json.dumps(alloc) + "\n")
            for codec in ["none", "zstd"]:
                group = []
                for workers in [2, 8]:
                    for name, program in programs.items():
                        target = args.out / f"{scenario}-{codec}-{workers}-{name}"
                        command = [str(program), "migrate", "--source-chaindata", str(fixture / "source"),
                                   "--out", str(target), "--wrapped-ether-code", str(ROOT / "internal/migration/testdata/wrapped.hex"),
                                   "--workers", str(workers), "--cache-mb", "64", "--handles", "64", "--quiet", "--compression", codec]
                        if name != "baseline" or args.baseline_events:
                            command += ["--ovm-events", str(fixture / "events.json")]
                        if name.startswith("current-"):
                            command += ["--code-cache", name.removeprefix("current-")]
                        if alloc is not None:
                            command += ["--ovm-genesis-alloc", str(alloc_path)]
                        if scenario.startswith("retain-"):
                            command += ["--ovm-erc20-retain-list", str(retain)]
                        report = json.loads(subprocess.check_output(command, cwd=ROOT))
                        verified = json.loads(subprocess.check_output([str(verifiers[name]), str(target)], cwd=ROOT))
                        if report["summary"] != verified:
                            raise RuntimeError("independent parity verification differs")
                        item = dict(scenario=scenario, codec=codec, workers=workers, implementation=name,
                                    encoding="baseline" if name == "baseline" else "current", summary=verified, final_root=verified["FinalStateRoot"], independently_verified=True,
                                    artifact_sha256=hashlib.sha256(target.read_bytes()).hexdigest(), artifact_bytes=target.stat().st_size)
                        group.append(item)
                        observations.append(item)
                check_observations(group, args.compare)
            if len({json.dumps(item["summary"], sort_keys=True) for item in observations if item["scenario"] == scenario}) != 1:
                raise RuntimeError("parity summaries differ across codecs")
        source_after = source_identity(fixture / "source")
        if source_before != source_after:
            raise RuntimeError("source database changed")
    result = dict(source_before=source_before, source_after=source_after, event_json_sha256=events_hash, scenarios=SCENARIOS, retain_address=RETAIN, accounts=10000, comparison=args.compare,
                  binary_sha256={name: hashlib.sha256(path.read_bytes()).hexdigest() for name, path in programs.items()},
                  verifier_sha256={name: hashlib.sha256(path.read_bytes()).hexdigest() for name, path in verifiers.items()},
                  observations=observations)
    (args.out / "parity.json").write_text(json.dumps(result, indent=2) + "\n")
    print(f"Verified {len(observations)} parity artifacts including cross-worker determinism")


if __name__ == "__main__":
    main()
