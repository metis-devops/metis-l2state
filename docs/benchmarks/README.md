# Latest benchmark suite: ovm-event-sync

Completed 2026-10-22 (Asia/Shanghai). Baseline: `db706b9be6c7b42318548bd9aeeb8a9d2517533e`, the production code before this change. Baseline sources were extracted with `git archive`; only the identical candidate `sync_benchmark_test.go` harness was added to that temporary checkout. No baseline production code or reference migration repository was modified.

## Environment and method

Apple M3 Max (Mac15,9), 48 GiB RAM, macOS 27.0.1 build 26A434, go version go1.27.3 darwin/arm64. Test executables were built with `GOFLAGS=-buildvcs=false go test -c`. Runtime `GOGC`, `GOMEMLIMIT`, `GOMAXPROCS`, `GOEXPERIMENT` and `GOFLAGS` were unset. The raw report retains environment, source manifests, harness hash and executable hashes.

Three event densities, six rounds each, two implementations: 36 fresh-process samples. Each sample synchronizes 2,000 blocks from genesis in two batches of 1,000 through a localhost HTTP JSON-RPC server without injected delay. Empty has no logs; sparse emits Approval and nonzero Transfer every 200 blocks (20 logs); dense emits both in every block (4,000 logs). Each owner/sender is distinct. Baseline/current execution order alternates each round. Runs are serial, caches are not flushed, and no CI/race or unrelated benchmark jobs ran concurrently with these samples.

The Go timer includes synchronization and durable checkpoint saving. Chain/server setup and output verification are outside that timer. Expected checkpoint bytes are constructed directly from the generated records, compared byte-for-byte, and independently strictly decoded after timing. Process wall time, CPU and peak RSS include setup/verification and are recorded separately; RSS is measured by the OS. All samples, including first and slow samples, are retained.

## Results

| Event density | Baseline median ms | Current median ms | Change | Header calls baseline → current | Log calls |
|---|---:|---:|---:|---:|---:|
| empty | 183.404 | 10.039 | -94.53% | 2007 → 11 | 2 |
| sparse | 193.987 | 10.924 | -94.37% | 2007 → 11 | 2 |
| dense | 249.715 | 76.718 | -69.28% | 2007 → 11 | 2 |

Header calls fall from 2,007 to 11 (99.45% fewer), independent of event density. These totals include chain identity, latest-head and checkpoint checks. Resumed batch attempts make four boundary header calls: checkpoint and endpoint before/after logs. Unit tests separately assert that count for 3-, 100- and 10,000-block batches and verify no interior header requests.

| Event density | Baseline allocated bytes/op | Current allocated bytes/op | Baseline allocs/op | Current allocs/op | Baseline process peak RSS MiB | Current process peak RSS MiB |
|---|---:|---:|---:|---:|---:|---:|
| empty | 69472100 | 659600 | 791186 | 5848 | 24.61 | 19.00 |
| sparse | 70337576 | 1094636 | 795217 | 9512 | 24.73 | 20.21 |
| dense | 123171584 | 59500084 | 1425528 | 644934 | 35.51 | 34.23 |

## Correctness and limits

All 36 samples matched independently constructed checkpoint bytes; baseline/current SHA-256 values match in every density. The change deliberately trusts RPC canonicality for interior logs rather than fetching their headers. It retains range, nonzero hash, encoding, duplicate-position and same-block hash-consistency checks, and matches genesis/endpoint logs against known hashes. Checkpoint and endpoint changes stop synchronization before committing a batch.

Tests cover empty/dense/sparse ranges, checkpoint and endpoint reorgs, retry/range-reduction checkpoint revalidation, genesis mismatch, malformed/out-of-range/conflicting logs, repeated senders, cancellation through the final boundary check, ordering, resume and atomic batch commit. `make ci`, `make test-race`, six existing Python benchmark tests and `git diff --check` passed. This is local synthetic HTTP evidence; no public RPC, remote CI or production validation was performed.

The unrelated migration/dump campaign was stopped at the user’s request. Its partial results were discarded and are not presented as a completed suite. No code-index or mainnet performance claim is made. The generic migration benchmark harness remains unchanged.

## Raw results and reproduction

- [paired.json](paired.json): all 36 timing, request-count, allocation, CPU/RSS samples and source/binary identities.
- [allocations.txt](allocations.txt): complete raw output from every sync benchmark process.
- [parity.json](parity.json): verification method and matching checkpoint hashes.

Run from the candidate checkout with a fresh output directory:

```sh
baseline=$(mktemp -d)
git archive db706b9be6c7b42318548bd9aeeb8a9d2517533e | tar -x -C "$baseline"
cp internal/ovmevents/sync_benchmark_test.go "$baseline/internal/ovmevents/"
(cd "$baseline" && GOFLAGS=-buildvcs=false go test -c -o ovmevents.test ./internal/ovmevents)
GOFLAGS=-buildvcs=false go test -c -o /tmp/current-ovmevents.test ./internal/ovmevents
PYTHONDONTWRITEBYTECODE=1 python3 scripts/benchmark_sync.py \
  --baseline "$baseline/ovmevents.test" --current /tmp/current-ovmevents.test \
  --baseline-ref db706b9be6c7b42318548bd9aeeb8a9d2517533e \
  --pairs 6 --out /new/sync-benchmark-suite
```

The optional `--baseline-identity` accepts the archived baseline source manifest. This campaign’s manifest and exact copied harness hash are retained in `paired.json.baseline_identity`. This README and its three raw files replace superseded reports together; only the latest completed sync suite is retained, without removing benchmark/test code.
