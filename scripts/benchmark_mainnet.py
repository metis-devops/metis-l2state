#!/usr/bin/env python3
"""Paired fresh-process migration of a stopped, restored operator fixture.

Run only after reconstruction and profiling. Verification, hashing and cleanup
are outside timing; every sample (including slow samples) is retained.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import time


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def manifest(directory):
    return {p.name: {'bytes': p.stat().st_size, 'sha256': digest(p)}
            for p in sorted(directory.iterdir()) if p.is_file()}


def measure(command, stdout, stderr):
    with stdout.open('w') as out, stderr.open('w') as err:
        started = time.monotonic()
        process = subprocess.Popen(command, stdout=out, stderr=err)
        _, status, usage = os.wait4(process.pid, 0)
        seconds = time.monotonic() - started
        process.returncode = os.waitstatus_to_exitcode(status)
    if process.returncode:
        raise RuntimeError(stderr.read_text())
    return dict(seconds=seconds, user_seconds=usage.ru_utime, system_seconds=usage.ru_stime,
                peak_rss_bytes=usage.ru_maxrss * (1 if platform.system() == 'Darwin' else 1024),
                command=command)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ['baseline', 'current', 'verifier', 'source', 'fixtures', 'runtime', 'out']:
        p.add_argument('--' + name, type=Path, required=True)
    p.add_argument('--pairs', type=int, default=3)
    p.add_argument('--workers', type=int, default=8)
    p.add_argument('--cache-mb', type=int, default=512)
    p.add_argument('--handles', type=int, default=256)
    p.add_argument('--control-cache-mb', type=int, default=0,
                   help='optional baseline-only resource control cache budget')
    p.add_argument('--control-handles', type=int, default=0,
                   help='optional baseline-only resource control handle budget')
    args = p.parse_args()
    for name in ['baseline', 'current', 'verifier', 'source', 'fixtures', 'runtime', 'out']:
        setattr(args, name, getattr(args, name).resolve())
    if args.pairs < 2:
        p.error('at least two pairs are required')
    if args.cache_mb <= 0 or args.handles <= 0 or not 2 <= args.workers <= 16:
        p.error('positive cache/handles and 2-16 workers are required')
    if args.control_cache_mb < 0 or args.control_handles < 0:
        p.error('control cache/handles cannot be negative')
    args.out.mkdir(parents=True, exist_ok=False)
    inputs = manifest(args.fixtures)
    source = manifest(args.source)
    payload = dict(platform=platform.platform(), go_version=subprocess.check_output(['go', 'version'], text=True).strip(),
                   environment={k: os.environ.get(k) for k in ['GOGC', 'GOMEMLIMIT', 'GOMAXPROCS', 'GOEXPERIMENT']},
                   fixtures=inputs, source=source, runtime_sha256=digest(args.runtime),
                   binaries={k: digest(getattr(args, k)) for k in ['baseline', 'current', 'verifier']},
                   samples=[], pairs=args.pairs, workers=args.workers, cache_mb=args.cache_mb, handles=args.handles,
                   method='forward/reverse variant order; serial fresh processes; caches not flushed; hash/readback outside timing')
    reference = None
    policies = {name: {'binary': name, 'cache_mb': args.cache_mb, 'handles': args.handles}
                for name in ['baseline', 'current']}
    if args.control_cache_mb or args.control_handles:
        policies['baseline-control'] = {'binary': 'baseline',
                                        'cache_mb': args.control_cache_mb or args.cache_mb,
                                        'handles': args.control_handles or args.handles}
    variants = list(policies)
    payload['variants'] = policies
    for pair in range(args.pairs):
        order = variants if pair % 2 == 0 else list(reversed(variants))
        for implementation in order:
            name = f'{pair}-{implementation}'
            artifact = args.out / (name + '.state')
            policy = payload['variants'][implementation]
            command = [str(getattr(args, policy['binary'])), 'migrate', '--source-chaindata', str(args.source),
                       '--out', str(artifact), '--ovm-events', str(args.fixtures / 'ovm-events.json'),
                       '--ovm-audit-summary', str(args.fixtures / 'ovm-audit-report.json'),
                       '--wrapped-ether-code', str(args.runtime), '--workers', str(args.workers),
                       '--cache-mb', str(policy['cache_mb']), '--handles', str(policy['handles']), '--compression', 'zstd', '--code-cache', 'memory']
            print(f'mainnet pair={pair} {implementation}', flush=True)
            sample = measure(command, args.out / (name + '.json'), args.out / (name + '.log'))
            summary = json.loads((args.out / (name + '.json')).read_text())['summary']
            verified = json.loads(subprocess.check_output([str(args.verifier), str(artifact)], text=True))
            if summary != verified:
                raise RuntimeError('independent summary mismatch')
            identity = dict(sha256=digest(artifact), bytes=artifact.stat().st_size, summary=summary)
            if reference is None:
                reference = identity
            if identity != reference:
                raise RuntimeError('baseline/candidate artifact differs')
            sample.update(pair=pair, implementation=implementation, artifact=identity, independently_verified=True)
            payload['samples'].append(sample)
            (args.out / 'results.json').write_text(json.dumps(payload, indent=2) + '\n')
            artifact.unlink()
            print(f'  {sample["seconds"]:.3f}s RSS={sample["peak_rss_bytes"] / 2**20:.1f}MiB', flush=True)
    if manifest(args.source) != source or manifest(args.fixtures) != inputs:
        raise RuntimeError('source or input files changed')
    payload['source_unchanged'] = payload['fixtures_unchanged'] = True
    (args.out / 'results.json').write_text(json.dumps(payload, indent=2) + '\n')


if __name__ == '__main__':
    main()
