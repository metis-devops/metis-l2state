# Latest local measurements — 2026-10-20

This campaign restores the operator-provided mainnet state at height **23,182,122**, profiles migration, and compares bounded iterator batching against clean task-start HEAD `c61d52a7ece259526edff5f0ad5a254b0619805c`. The change amortizes worker-permit acquisition over at most 32 trie leaves or preimage records. Every record remains validated and cancellation checked. Producers release permits before queue sends; the 64-entry queues remain bounded, with up to 32 additional producer-local leaves per partition. Source validation, OVM accounting, ordered B+trees, code IDs, acknowledged release, alloc, format v1 and publication semantics are unchanged. Dependency versions and the frozen corpus were not changed.

## Baseline, environment and scope

Apple M3 Max (Mac15,9), 48 GiB RAM, macOS 27.0.1 build 26A434, Go 1.27.2 darwin/arm64. `GOGC`, `GOMEMLIMIT`, `GOMAXPROCS` and `GOEXPERIMENT` were unset. Both sides use `GOFLAGS=-buildvcs=false`, CLI/verifier builds use `go build -trimpath`, and allocation tests use `go test -c`. `paired.json` retains environment, clean baseline identity, source manifests and binary hashes. The reference migration repository was not used or modified.

All timed invocations run serially in fresh processes. Caches are not flushed; the OS page cache is shared. Fixture preparation, input hashing, independent output verification and artifact deletion are outside timing. Every successful sample, including first and slow samples, is retained. Peak RSS is measured per process by the OS; synthetic output/scratch disk peaks are sampled every 20 ms and are not exact maxima. No CI/race tests ran concurrently with timing. These are local reconstructed-mainnet-data and synthetic measurements; no remote CI, original-node layout benchmark or production migration was run.

## Reconstruction and correctness

`testdata/restorestate` consumes the original-state dump in order and constructs a new legacy LevelDB with hashed account/storage tries, legacy bare-hash code, all 32,649,233 exported preimages, canonical genesis/target header RLP and head mappings. The rebuilt DB uses the legacy Bloom filter. The supplied target header must match the embedded dump header byte-for-byte. The database is closed before all subsequent strictly read-only operations. This restores current state only: historical blocks, receipts and the genesis trie are not reconstructed, and it is not a runnable full-node backup.

| Identity | Value |
| --- | --- |
| Height | 23,182,122 |
| Head hash | `0x31ccdca9cb607cdde996f7a8a82519bc873ffff68b898a3d8472069fc5724226` |
| Source root | `0x3bfc6320e507cc55567c2e0e524a33039af91706a2daefc627827f4992735460` |
| Restored accounts / slots / distinct codes | 1,862,948 / 30,638,364 / 19,882 |
| Restored code bytes | 160,962,644 |

The independently pinned old l2geth trie implementation read every restored account, storage trie and referenced code, verified address preimages and canonical head identity, and matched these inventories (296.35 seconds, outside timing). Original fixture and restored DB file digests remained unchanged. All supplied input digests and the independent readback log are retained in `paired.json`.

Mainnet-data migrations use the supplied RPC events and audit report, **test runtime `internal/migration/testdata/wrapped.hex`**, no alloc, no manually retained addresses, eight workers, zstd and memory code cache. They test conversion/accounting on real state; the resulting artifact is not a deployment-ready runtime selection. Source header RLP retains the original state root.

All complete profiled and timed mainnet outputs are byte-identical and independently verified: SHA-256 `c095eefb1cb6a548e2fbc8a07d0d2ce49afb50e521335ef68a75ccacca75968e`, 1,234,263,400 bytes; converted/final root `0xed5b0350bbb9e4cfa612229ad77c6aefd035511f3427999e219dcfdf6260f2ae`; 2,219,875 accounts, 29,288,487 slots, 19,882 codes and 160,960,292 code bytes. Complete conversion accounting is retained in raw summaries.

## Profiles and bottlenecks

Profiles used the same 512 MiB cache / 2,048 handles / eight workers on both sides; CPU, allocation, heap, mutex and block profiles were collected together. Baseline source validation took 509.603 s of 563.624 s total (~90%); conversion 4.172 s and output 48.671 s. Candidate totals were 515.974 s, including 462.069 s validation, 4.198 s conversion and 48.478 s output. These single instrumented observations are diagnostics, not paired speedup estimates.

Baseline CPU samples attributed 57.43% to `syscall.rawsyscalln`, 19.57% to `pthread_cond_wait` and 12.89% to `pthread_cond_signal`; LevelDB Get accounted for 54.24% cumulative sampled CPU. Sampled allocation space was 114.38 GiB, including 27.4 GiB in LevelDB `newBlockIter`. This points to source reads/decompression and repeated scheduling/permit handoffs as the useful targets, rather than OVM conversion. Candidate CPU samples reduced cumulative pthread wait from 155.84 s to 59.36 s and signal from 102.63 s to 49.37 s, while raw syscall samples remained approximately 457 s / 455 s. This supports the scheduling-overhead diagnosis without claiming fewer database reads. Aggregated lock/block times include concurrent waiting and bounded-queue backpressure; they are not wall time.

Full MemStats allocated bytes were 122,745,755,544 baseline and 122,605,979,000 candidate; mallocs 1,765,515,647 and 1,761,871,335. Allocation volume is essentially unchanged. Post-run heap values are not peak memory. Profile text, sampling settings, logs and profile hashes are retained in `paired.json.query_analysis`.

An initial 64 MiB / 64-handle diagnostic was interrupted after at least 581.146 seconds while still validating. It produced no complete migration artifact or usable completed CPU profile. Its log, elapsed lower bound and system sample are preserved as an incomplete diagnostic, not included in successful timed statistics. An early unfiltered reconstruction was superseded by a separately created Bloom-filter DB; the source was never repaired or compacted.

## Mainnet-data paired timings

Two rounds use forward/reverse order: baseline → current → baseline-control, then baseline-control → current → baseline. Baseline and current use identical 8,192 MiB cache / 4,096 handle budgets. Baseline-control uses the same baseline binary with 512 MiB / 2,048 handles. The resource comparison changes both budgets together and cannot attribute their individual effects. With only two rounds these measurements describe this machine and restored layout; they do not establish broad statistical confidence. Defaults remain unchanged.

| Variant | Cache MiB / handles | All wall samples (s) | Median wall (s) | Median CPU (s) | Median peak RSS (GiB) |
| --- | --- | --- | --- | --- | --- |
| baseline-control | 512 / 2048 | 517.681 / 499.106 | 508.394 | 786.587 | 7.138 |
| baseline | 8192 / 4096 | 386.160 / 332.409 | 359.285 | 633.799 | 18.968 |
| current | 8192 / 4096 | 345.912 / 303.638 | 324.775 | 497.070 | 17.241 |

At the same resource budget, candidate median wall changes by -9.61%. Raising the baseline resource budget changes its median wall by -29.33%; this is a separate configuration tradeoff. The combined candidate/high-budget comparison against baseline-control changes wall by -36.12%. Full per-sample CPU, peak RSS, commands, phase logs and artifact identities are in `paired.json.mainnet`.

## Complete synthetic suite

- [paired.json](paired.json): 558 CLI samples (31 configurations × six rounds × three variants), 108 code-index samples, all six mainnet CLI samples, diagnostics, manifests and executable identities.
- [allocations.txt](allocations.txt): 558 separate fresh-process allocation samples, 108 code-index samples and both mainnet diagnostic MemStats records.
- [parity.json](parity.json): 60 retain/alloc artifacts and 64 freshly rerun baseline/candidate metadata-boundary pairs (128 artifacts), with the boundary test source.

All 558 CLI outputs match baseline bytes and full summaries and pass independent Reader verification. Retain/alloc checks cover workers 2/8, both codecs and both candidate code caches. Metadata boundaries cover 31, 32, 1 MiB and 1 MiB + 1 byte lengths across migrate/dump, workers 2/16, both codecs and caches. Source digests are unchanged.

Each synthetic configuration uses all six order permutations, with a 64 MiB source cache and 64 file handles. Baseline uses command defaults (migrate memory, dump none); candidate caches are explicit. `none` in configuration names is compression. `1k-bytecode` has 1,024 distinct 65,536-byte codes; other code workloads use 10-byte codes. `32x1025-storage` crosses the partition threshold; `1m-storage` contains one million slots. `-alloc` is a no-op existing-account overlay and `-overlay` clears code and edits storage.

Values below are medians of six samples. B/M/N denotes baseline/current-memory/current-none. Wall and CPU are milliseconds; peak RSS is MiB. Compare M for migrate and N for dump to isolate the code change; other comparisons also change code-cache policy.

| Configuration | Wall ms B / M / N | Wall Δ M / N | CPU ms B / M / N | Peak RSS MiB B / M / N |
| --- | --- | --- | --- | --- |
| 1k-bytecode-w8-zstd | 268.63 / 268.33 / 392.08 | -0.11% / +45.96% | 476.68 / 470.03 / 589.82 | 323.70 / 324.96 / 198.36 |
| 1k-bytecode-w8-none | 250.58 / 253.57 / 378.13 | +1.19% / +50.90% | 459.15 / 456.50 / 575.22 | 281.27 / 302.31 / 177.90 |
| 10k-w2-zstd | 144.40 / 143.12 / 143.66 | -0.89% / -0.51% | 293.32 / 285.14 / 283.21 | 54.84 / 55.62 / 55.00 |
| 10k-w8-zstd | 137.85 / 132.78 / 131.33 | -3.68% / -4.73% | 346.26 / 318.86 / 310.49 | 56.12 / 57.92 / 56.77 |
| 10k-w8-none | 138.10 / 135.11 / 132.48 | -2.16% / -4.07% | 352.06 / 332.08 / 320.12 | 54.80 / 56.27 / 54.62 |
| 100k-w8-zstd | 1416.07 / 1353.30 / 1356.27 | -4.43% / -4.22% | 3472.48 / 3135.14 / 3112.06 | 261.17 / 269.50 / 260.95 |
| 10k-codes-w8-zstd | 174.68 / 165.81 / 187.26 | -5.08% / +7.20% | 515.46 / 502.78 / 531.69 | 74.34 / 76.28 / 77.83 |
| 10k-codes-w8-none | 164.53 / 157.09 / 174.75 | -4.52% / +6.21% | 488.40 / 484.31 / 494.44 | 59.89 / 61.83 / 60.71 |
| 100k-codes-w8-zstd | 1836.61 / 1806.66 / 2053.47 | -1.63% / +11.81% | 4987.29 / 5148.40 / 5297.33 | 302.01 / 301.29 / 301.13 |
| 100k-codes-w8-none | 1795.85 / 1763.03 / 2004.01 | -1.83% / +11.59% | 4990.38 / 5138.98 / 5304.44 | 281.93 / 282.78 / 281.55 |
| 10k-codes-w8-zstd-alloc | 178.59 / 173.55 / 190.43 | -2.83% / +6.63% | 500.92 / 500.19 / 530.02 | 75.85 / 77.83 / 77.87 |
| 10k-codes-w8-none-alloc | 175.71 / 171.88 / 187.94 | -2.18% / +6.96% | 512.92 / 510.70 / 528.23 | 59.16 / 61.67 / 60.62 |
| 100k-codes-w8-zstd-alloc | 1934.47 / 1913.12 / 2150.83 | -1.10% / +11.18% | 5175.34 / 5320.48 / 5466.73 | 301.84 / 302.74 / 301.16 |
| 100k-codes-w8-none-alloc | 1873.12 / 1782.17 / 2117.67 | -4.86% / +13.06% | 5106.00 / 5008.81 / 5573.43 | 309.00 / 283.16 / 281.09 |
| 32x1025-storage-w8-zstd | 159.21 / 115.60 / 115.68 | -27.40% / -27.34% | 583.51 / 454.71 / 442.92 | 37.44 / 36.66 / 36.55 |
| 1m-storage-w8-zstd | 14385.42 / 12318.58 / 12248.14 | -14.37% / -14.86% | 22030.81 / 15500.61 / 15453.19 | 340.06 / 342.37 / 336.74 |
| 1m-storage-w8-none | 14360.94 / 12198.97 / 12189.34 | -15.05% / -15.12% | 21961.81 / 15421.21 / 15376.64 | 325.71 / 319.55 / 315.80 |
| 1m-storage-w8-zstd-overlay | 14538.38 / 12435.20 / 12327.48 | -14.47% / -15.21% | 22709.66 / 16156.11 / 16083.92 | 341.74 / 341.34 / 336.16 |
| dump-1k-bytecode-w8-zstd | 388.73 / 267.20 / 389.29 | -31.26% / +0.14% | 589.45 / 468.95 / 580.35 | 196.80 / 323.82 / 196.67 |
| dump-1k-bytecode-w8-none | 373.55 / 248.33 / 374.94 | -33.52% / +0.37% | 571.89 / 453.44 / 571.87 | 176.43 / 300.54 / 176.09 |
| dump-10k-w2-zstd | 118.19 / 113.57 / 112.79 | -3.91% / -4.57% | 256.35 / 235.16 / 230.92 | 52.64 / 54.21 / 53.44 |
| dump-10k-w8-zstd | 102.46 / 100.59 / 99.04 | -1.82% / -3.34% | 272.11 / 255.55 / 253.13 | 56.53 / 56.73 / 55.75 |
| dump-10k-w8-none | 96.34 / 95.67 / 93.55 | -0.70% / -2.90% | 260.31 / 249.43 / 246.58 | 42.76 / 45.10 / 44.00 |
| dump-100k-w8-zstd | 1027.12 / 1029.23 / 1020.71 | +0.21% / -0.62% | 3586.41 / 3779.50 / 3750.36 | 208.61 / 209.95 / 207.98 |
| dump-10k-codes-w8-zstd | 147.49 / 124.23 / 140.57 | -15.77% / -4.69% | 455.73 / 397.65 / 407.42 | 67.39 / 70.53 / 69.13 |
| dump-10k-codes-w8-none | 138.72 / 112.51 / 131.18 | -18.90% / -5.44% | 463.46 / 388.11 / 409.62 | 50.20 / 52.30 / 51.13 |
| dump-100k-codes-w8-zstd | 1767.61 / 1536.05 / 1815.29 | -13.10% / +2.70% | 6060.02 / 6910.69 / 7172.33 | 260.97 / 264.12 / 261.28 |
| dump-100k-codes-w8-none | 1685.66 / 1461.20 / 1718.36 | -13.32% / +1.94% | 5925.31 / 6819.22 / 7165.10 | 239.82 / 241.14 / 239.08 |
| dump-32x1025-storage-w8-zstd | 122.77 / 85.07 / 86.08 | -30.70% / -29.88% | 525.98 / 410.07 / 414.52 | 50.00 / 51.28 / 49.59 |
| dump-1m-storage-w8-zstd | 13145.49 / 11100.78 / 11140.09 | -15.55% / -15.26% | 20971.06 / 14296.57 / 14417.67 | 213.42 / 213.48 / 212.77 |
| dump-1m-storage-w8-none | 13194.38 / 11153.64 / 11073.13 | -15.47% / -16.08% | 21051.29 / 14443.09 / 14337.92 | 191.98 / 194.11 / 191.84 |

The optimization has workload tradeoffs. With unchanged default cache policies, dump with 100,000 distinct codes increased median wall by 2.70% (zstd) / 1.94% (uncompressed), and CPU by 18.36% / 20.92%. The uncompressed 1,024-large-code migration increased wall by 1.19% and peak RSS by 7.48%. These results are retained, not dismissed as outliers. The measured benefit targets mainnet migration and storage-heavy traversal; it is not a universal CPU, memory or throughput improvement. No hard RSS guarantee or default resource increase is introduced.

All slow samples remain in raw data. Maxima are reported separately:

| Variant | Slowest configuration / pair | Seconds | Maximum peak RSS MiB |
| --- | --- | --- | --- |
| baseline | 1m-storage-w8-zstd-overlay / 1 | 14.584 | 346.30 |
| current-memory | 1m-storage-w8-zstd-overlay / 0 | 13.078 | 353.56 |
| current-none | 1m-storage-w8-zstd-overlay / 2 | 12.499 | 338.66 |

## Allocation and code-index controls

Allocation measurements use a separate fixed fixture (10,000 contract storage slots, workers 2, 16 MiB cache, zstd, benchtime 1x) in each CLI phase. They do not represent that phase’s CLI account/code/storage/codec dimensions. Each raw record is preserved with phase, pair and implementation. The code index is unchanged; nine workloads use six alternating baseline/current fresh-process pairs. Medians follow as ms/op / B/op / allocs/op.

| Allocation phase | Baseline ms / B / allocs | Current memory ms / B / allocs | Current none ms / B / allocs |
| --- | --- | --- | --- |
| 1k-bytecode-w8-zstd | 40.966 / 44,185,308 / 245,724.5 | 39.879 / 44,144,928 / 244,658.5 | 39.919 / 43,101,884 / 244,718.0 |
| 1k-bytecode-w8-none | 41.462 / 44,186,148 / 245,716.0 | 40.415 / 44,146,992 / 244,662.0 | 40.382 / 43,103,304 / 244,728.0 |
| 10k-w2-zstd | 40.855 / 44,194,356 / 245,745.0 | 39.890 / 44,149,060 / 244,684.0 | 41.331 / 43,107,052 / 244,754.5 |
| 10k-w8-zstd | 41.966 / 44,194,492 / 245,716.0 | 39.855 / 44,152,340 / 244,720.0 | 40.468 / 43,105,964 / 244,728.5 |
| 10k-w8-none | 40.926 / 44,190,980 / 245,705.5 | 39.956 / 44,149,504 / 244,678.5 | 39.967 / 43,103,612 / 244,741.5 |
| 100k-w8-zstd | 41.864 / 44,187,988 / 245,705.5 | 40.913 / 44,145,008 / 244,690.0 | 40.907 / 43,097,252 / 244,705.0 |
| 10k-codes-w8-zstd | 40.957 / 44,187,736 / 245,743.5 | 40.856 / 44,147,312 / 244,701.5 | 40.776 / 43,097,060 / 244,684.0 |
| 10k-codes-w8-none | 41.930 / 44,187,552 / 245,700.5 | 40.411 / 44,149,068 / 244,655.5 | 41.456 / 43,099,568 / 244,728.5 |
| 100k-codes-w8-zstd | 39.955 / 44,189,276 / 245,724.0 | 40.422 / 44,150,068 / 244,690.5 | 39.905 / 43,099,276 / 244,707.0 |
| 100k-codes-w8-none | 40.879 / 44,184,924 / 245,705.0 | 39.913 / 44,147,660 / 244,663.0 | 39.909 / 43,102,824 / 244,679.5 |
| 10k-codes-w8-zstd-alloc | 41.842 / 44,189,348 / 245,726.0 | 40.882 / 44,154,396 / 244,694.5 | 39.872 / 43,096,700 / 244,677.5 |
| 10k-codes-w8-none-alloc | 40.870 / 44,184,820 / 245,699.5 | 39.921 / 44,149,200 / 244,688.0 | 40.435 / 43,103,388 / 244,692.5 |
| 100k-codes-w8-zstd-alloc | 41.869 / 44,185,440 / 245,689.0 | 40.883 / 44,146,244 / 244,644.5 | 40.392 / 43,103,620 / 244,742.0 |
| 100k-codes-w8-none-alloc | 41.859 / 44,187,012 / 245,722.5 | 40.802 / 44,149,340 / 244,678.0 | 40.425 / 43,104,056 / 244,743.0 |
| 32x1025-storage-w8-zstd | 41.881 / 44,192,004 / 245,717.5 | 40.923 / 44,151,676 / 244,701.5 | 40.901 / 43,110,248 / 244,812.0 |
| 1m-storage-w8-zstd | 40.882 / 44,193,216 / 245,774.5 | 39.928 / 44,156,088 / 244,720.5 | 39.937 / 43,106,240 / 244,714.5 |
| 1m-storage-w8-none | 41.922 / 44,185,740 / 245,721.0 | 41.338 / 44,152,032 / 244,713.5 | 41.425 / 43,102,828 / 244,745.0 |
| 1m-storage-w8-zstd-overlay | 40.911 / 44,186,304 / 245,722.0 | 39.896 / 44,150,544 / 244,688.5 | 39.925 / 43,098,520 / 244,751.0 |
| dump-1k-bytecode-w8-zstd | 28.959 / 38,780,792 / 204,462.0 | 28.834 / 39,835,216 / 204,478.0 | 28.868 / 38,782,452 / 204,469.5 |
| dump-1k-bytecode-w8-none | 30.342 / 38,785,376 / 204,468.0 | 28.925 / 39,834,248 / 204,473.0 | 28.905 / 38,781,600 / 204,459.0 |
| dump-10k-w2-zstd | 31.388 / 38,786,836 / 204,467.0 | 29.367 / 39,839,660 / 204,471.0 | 29.428 / 38,781,744 / 204,468.0 |
| dump-10k-w8-zstd | 30.926 / 38,784,840 / 204,481.5 | 29.905 / 39,834,696 / 204,468.5 | 28.896 / 38,786,812 / 204,475.5 |
| dump-10k-w8-none | 30.900 / 38,781,932 / 204,476.5 | 29.927 / 39,840,288 / 204,492.0 | 28.913 / 38,790,260 / 204,482.5 |
| dump-100k-w8-zstd | 28.908 / 38,785,400 / 204,485.0 | 28.868 / 39,836,148 / 204,476.5 | 28.913 / 38,782,224 / 204,456.0 |
| dump-10k-codes-w8-zstd | 30.797 / 38,785,748 / 204,483.5 | 29.398 / 39,838,372 / 204,478.5 | 30.353 / 38,781,864 / 204,468.5 |
| dump-10k-codes-w8-none | 30.887 / 38,786,400 / 204,457.5 | 29.903 / 39,839,900 / 204,486.5 | 28.934 / 38,781,284 / 204,458.0 |
| dump-100k-codes-w8-zstd | 29.875 / 38,785,772 / 204,491.5 | 29.437 / 39,838,140 / 204,489.5 | 29.981 / 38,781,200 / 204,471.5 |
| dump-100k-codes-w8-none | 30.850 / 38,786,708 / 204,483.5 | 28.909 / 39,838,800 / 204,484.5 | 28.928 / 38,777,208 / 204,441.0 |
| dump-32x1025-storage-w8-zstd | 30.890 / 38,781,580 / 204,478.5 | 29.402 / 39,842,528 / 204,498.5 | 28.893 / 38,785,696 / 204,463.5 |
| dump-1m-storage-w8-zstd | 28.889 / 38,789,552 / 204,492.0 | 29.065 / 39,836,616 / 204,476.5 | 28.363 / 38,784,580 / 204,482.0 |
| dump-1m-storage-w8-none | 31.901 / 38,789,392 / 204,488.0 | 30.392 / 39,834,796 / 204,481.0 | 31.340 / 38,781,872 / 204,449.5 |

Code-index medians:

| Distinct hashes / mode | Baseline | Current |
| --- | --- | --- |
| 10,000 / shared | 0.649 / 1,978,792 / 99.0 | 0.639 / 1,978,792 / 99.0 |
| 10,000 / overlap | 0.693 / 1,978,792 / 99.0 | 0.696 / 1,978,792 / 99.0 |
| 10,000 / repeated | 2.049 / 1,978,792 / 99.0 | 2.048 / 1,978,792 / 99.0 |
| 100,000 / shared | 6.238 / 16,609,192 / 641.0 | 6.177 / 16,609,192 / 641.0 |
| 100,000 / overlap | 6.680 / 16,609,192 / 641.0 | 6.734 / 16,609,192 / 641.0 |
| 100,000 / repeated | 22.528 / 16,609,192 / 641.0 | 22.546 / 16,609,192 / 641.0 |
| 1,000,000 / shared | 100.931 / 241,175,820 / 9,194.5 | 98.724 / 241,178,720 / 9,195.0 |
| 1,000,000 / overlap | 106.674 / 241,126,748 / 9,193.5 | 106.630 / 241,178,608 / 9,197.0 |
| 1,000,000 / repeated | 601.029 / 241,123,848 / 9,190.5 | 567.897 / 240,976,352 / 9,184.5 |

## Local validation

`make ci`, `make test-race`, six Python benchmark tests and `git diff --check` passed. Added tests cover partial/exact/multiple iterator batches, distinct-value ownership and ordering, full-queue permit release, cancellation and consumer/visitor errors. Restoration tests cover source-root roundtrip, original hashed storage keys/code/header, existing destination rejection, header mismatch, truncated input cleanup and cancellation. Existing output admission, bounded queues and acknowledged-release regressions remain. The optional real-fixture legacy traversal and metadata-boundary checks ran separately outside timing.

## Reproduction

Use fresh destination directories and a stopped restored DB. Original fixtures are private operator inputs; their hashes are recorded in paired.json. Reconstruct the clean baseline from the recorded commit:

```sh
baseline=$(mktemp -d)
git archive c61d52a7ece259526edff5f0ad5a254b0619805c | tar -x -C "$baseline"
(cd "$baseline" && GOFLAGS=-buildvcs=false go build -trimpath -o l2state ./cmd/l2state)
(cd "$baseline" && GOFLAGS=-buildvcs=false go build -trimpath -o verifyfile ./testdata/verifyfile)
(cd "$baseline" && GOFLAGS=-buildvcs=false go test -c -o migration.test ./internal/migration)
GOFLAGS=-buildvcs=false go build -trimpath -o /tmp/current-l2state ./cmd/l2state
go run ./testdata/restorestate --dump fixtures/dump.zstd \
  --preimages fixtures/preimages.rlp.gz --genesis fixtures/genesis-header.json \
  --target fixtures/target-block-header.json --out /tmp/restored-chaindata
(cd testdata/legacyintegration && L2STATE_RESTORED_SOURCE=/tmp/restored-chaindata \
  L2STATE_RESTORED_CACHE_MB=8192 L2STATE_RESTORED_HANDLES=4096 \
  go test -count=1 -v -run '^TestRestoredLegacyState$')
PYTHONDONTWRITEBYTECODE=1 python3 scripts/benchmark_mainnet.py \
  --baseline "$baseline/l2state" --current /tmp/current-l2state \
  --verifier "$baseline/verifyfile" --source /tmp/restored-chaindata \
  --fixtures fixtures --runtime internal/migration/testdata/wrapped.hex \
  --pairs 2 --workers 8 --cache-mb 8192 --handles 4096 \
  --control-cache-mb 512 --control-handles 2048 --out /tmp/mainnet-pairs
PYTHONDONTWRITEBYTECODE=1 python3 scripts/benchmark_suite.py \
  --baseline-binary "$baseline/l2state" --baseline-verifier "$baseline/verifyfile" \
  --baseline-test-binary "$baseline/migration.test" \
  --baseline-ref c61d52a7ece259526edff5f0ad5a254b0619805c \
  --compare bytes --baseline-index ordered --pairs 6 --out /tmp/synthetic-suite
```

For diagnostic profiles, copy only the current `testdata/dumpprofile/main.go` helper into the baseline checkout (leave baseline production sources unchanged) and build the helper separately in both checkouts. Run with `--command migrate --source <DB> --out <new.state> --profiles <new-dir> --mode all --ovm-events fixtures/ovm-events.json --audit fixtures/ovm-audit-report.json --runtime internal/migration/testdata/wrapped.hex --workers 8 --cache-mb 512 --handles 2048 --allocation-sample-bytes 524288 --block-rate-ns 1000000 --mutex-fraction 64`. Verify artifacts separately with `testdata/verifyfile`; use `go tool pprof -top` for each profile and the matching checkout as source path.

For metadata boundaries, extract `metadata_boundary.test_source` from parity.json into a temporary file, map each checkout’s absolute `internal/migration/metadata_review_test.go` path to it using a Go overlay, and run `go test -count=1 -overlay <overlay.json> ./internal/migration -run ^TestMetadataReviewBoundary$ -v`. Compare all 64 `BOUNDARY` JSON rows. The test also checks source digests and independently reads each artifact.

This README and the three raw files replace the preceding suite together. Only this latest complete campaign is retained here; no slow sample or benchmark/test program was removed.
