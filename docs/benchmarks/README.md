# Latest local synthetic measurements — 2026-10-19

This complete suite compares the task-start B+tree implementation using blocking `outputMu.Lock` with immediate concurrent-output rejection using `TryLock`. Already-canceled output now returns its context error before ownership and leaves frozen state available. Both sides use the same B+trees, ordered construction, direct OVM mutation and acknowledged batch release. This suite measures the admission fix; it does not repeat the older map-versus-tree comparison. Format v1, dependencies, source validation, audit, retention, alloc and publication semantics are unchanged. The frozen corpus was not regenerated.

## Baseline and environment

Baseline `btree-lock-task-start-76672b21304c` is the complete task-start working tree, including staged, unstaged and untracked B+tree files, over HEAD `1b7bc72e6f77c9611612e182dade891c258715e8`. It was exported to an isolated temporary directory before edits. `paired.json.baseline_identity` preserves its reconstruction patch, SHA-256 and source manifest; historical benchmark reports are excluded. The user's index was preserved. The reference migration repository was not used or modified. Candidate sources are identified by `paired.json.source_sha256`; no source changed during timing.

Apple M3 Max, 48 GiB physical memory; `macOS-27.0.1-arm64-arm-64bit-Mach-O`; `go version go1.27.2 darwin/arm64`. Exact OS build, runtime environment and Go environment are retained in paired.json. `GOGC`, `GOMEMLIMIT`, `GOMAXPROCS` and `GOEXPERIMENT` were unset. Both sides use `GOFLAGS=-buildvcs=false`; CLI/verifier builds use `go build -trimpath`, test executables use `go test -c`. Test executable identities may include checkout paths.

| Executable | SHA-256 |
| --- | --- |
| current-memory CLI | a1ba91d2fb091eb2001c55810b6ad5162932b77db0105ca62998bee5616fec84 |
| current-none CLI | a1ba91d2fb091eb2001c55810b6ad5162932b77db0105ca62998bee5616fec84 |
| baseline CLI | 57b9bccb7e9a82c83c5cd4aa5e131439b5f11cc78156058e5198e79a4849bcb6 |
| current-memory verifier | 3460885f2c03f21fe1e780b29fc9b6249de7e8851bb253b4dcffb7fb45b2e781 |
| current-none verifier | 3460885f2c03f21fe1e780b29fc9b6249de7e8851bb253b4dcffb7fb45b2e781 |
| baseline verifier | 3460885f2c03f21fe1e780b29fc9b6249de7e8851bb253b4dcffb7fb45b2e781 |
| current-memory allocation test | 57f866da72002a1e2b6e694839d35d1b8ca36ae07ac51d26b130b881d6661f9f |
| current-none allocation test | 57f866da72002a1e2b6e694839d35d1b8ca36ae07ac51d26b130b881d6661f9f |
| baseline allocation test | 1f73de66f547fa917e1db04a4f0fed6fee9b32b34c7b4d919e62fa02cde11723 |

## Method and correctness

- [paired.json](paired.json): all 558 timed CLI samples, 108 code-index samples, complete summaries, source manifests and executable identities.
- [allocations.txt](allocations.txt): all 558 fresh-process allocation samples and 108 code-index samples.
- [parity.json](parity.json): 60 retain/alloc artifacts and 64 fresh baseline/candidate metadata-boundary pairs (128 artifacts), including the boundary test source.

All 31 CLI configurations use six rounds and all six baseline/current-memory/current-none order permutations. Each invocation runs in a fresh process against the same stopped synthetic fixture. Runs are serial, with no CI/race tests or other performance campaigns during timing. Caches are not flushed. Fixture generation, hashing and independent readback are outside timing. Every sample, including first and slow runs, is retained. OS peak RSS is measured per process; sampled disk peaks are not exact peaks.

Baseline uses command defaults (migrate memory, dump none); candidate cache modes are explicit. `none` in configuration names means compression. `1k-bytecode` uses 1,024 distinct 65,536-byte codes; other distinct-code configurations use 10-byte code. `32x1025-storage` crosses the storage partition threshold. `1m-storage` has 1,000,000 slots. `-alloc` is a no-op existing-account alloc; `-overlay` clears code and edits storage.

All 558 timed artifacts match baseline bytes and complete summaries and pass independent Reader verification. All 60 retain/alloc artifacts also match across workers 2/8, both codecs and both candidate cache modes. The 64 metadata-boundary pairs match at lengths 31, 32, 1 MiB and 1 MiB + 1 across migrate/dump, workers 2/16, both codecs and cache modes. Source-file digests remain unchanged. Boundary checks ran separately outside timing.

Local `make ci`, `make test-race`, six Python benchmark tests and `git diff --check` passed. New regressions fail against the baseline and pass with the fix; 25 race-enabled repetitions cover blocked Writer ownership, immediate rejection of live/canceled/expired contenders, successful and failed owner completion, and canceled admission preserving the snapshot for a later valid output. Existing cancellation, ordering, queue bounds, acknowledged release and independent legacy reconstruction tests remain. This is local synthetic and pinned integration evidence; no remote CI or production snapshot was run.

## Performance and peak memory

Values are medians of six samples: wall and CPU (user + system) in milliseconds, per-process peak RSS in MiB. B/M/N means baseline/current-memory/current-none. Percentages compare medians. For the admission change compare M for migrate and N for dump; other comparisons also change cache policy. CLI samples perform one output each, so immediate rejection latency is established by regression tests, not these throughput measurements.

migrate: default-cache wall median changes range from -2.44% to +4.22%; peak-RSS median changes range from -3.45% to +1.01%.

dump: default-cache wall median changes range from -0.37% to +1.57%; peak-RSS median changes range from -0.94% to +1.38%.

The admission check runs once per export and does not change tree storage or the main traversal. Treat timing/RSS variation as local measurements, not a demonstrated throughput or memory optimization. Dump is an unchanged-policy control. There is no forced GC, hard RSS bound or production performance claim.

| Configuration | Wall ms B / M / N | Wall Δ M / N | CPU ms B / M / N | Peak RSS MiB B / M / N | RSS Δ M / N |
| --- | --- | --- | --- | --- | --- |
| 1k-bytecode-w8-zstd | 267.67 / 267.41 / 392.03 | -0.10% / +46.46% | 481.93 / 481.25 / 597.98 | 323.88 / 324.39 / 200.20 | +0.16% / -38.19% |
| 1k-bytecode-w8-none | 251.35 / 251.61 / 374.38 | +0.10% / +48.95% | 462.85 / 463.51 / 579.33 | 282.84 / 285.69 / 179.45 | +1.01% / -36.55% |
| 10k-w2-zstd | 141.66 / 141.76 / 141.69 | +0.07% / +0.02% | 298.93 / 300.48 / 299.30 | 54.72 / 55.05 / 54.55 | +0.60% / -0.30% |
| 10k-w8-zstd | 135.75 / 139.55 / 138.19 | +2.80% / +1.80% | 344.16 / 355.50 / 345.45 | 55.98 / 55.96 / 55.16 | -0.04% / -1.48% |
| 10k-w8-none | 135.14 / 137.08 / 136.36 | +1.43% / +0.90% | 341.28 / 350.00 / 346.67 | 54.63 / 54.16 / 53.06 | -0.86% / -2.87% |
| 100k-w8-zstd | 1480.51 / 1479.24 / 1500.52 | -0.09% / +1.35% | 3665.57 / 3616.99 / 3695.15 | 260.91 / 260.62 / 258.51 | -0.11% / -0.92% |
| 10k-codes-w8-zstd | 179.66 / 175.27 / 199.63 | -2.44% / +11.11% | 519.93 / 511.29 / 544.64 | 74.19 / 72.83 / 75.52 | -1.83% / +1.80% |
| 10k-codes-w8-none | 166.53 / 167.89 / 186.71 | +0.81% / +12.12% | 495.69 / 510.78 / 527.02 | 60.32 / 59.83 / 58.99 | -0.82% / -2.20% |
| 100k-codes-w8-zstd | 1936.89 / 1931.23 / 2205.63 | -0.29% / +13.88% | 5222.32 / 5217.96 / 5451.64 | 301.68 / 300.96 / 299.79 | -0.24% / -0.63% |
| 100k-codes-w8-none | 1891.35 / 1971.22 / 2229.85 | +4.22% / +17.90% | 5223.51 / 5249.83 / 5599.44 | 309.22 / 308.80 / 295.73 | -0.13% / -4.36% |
| 10k-codes-w8-zstd-alloc | 189.75 / 191.09 / 210.02 | +0.71% / +10.68% | 544.11 / 543.44 / 562.37 | 75.61 / 73.00 / 76.21 | -3.45% / +0.80% |
| 10k-codes-w8-none-alloc | 185.36 / 186.34 / 206.02 | +0.53% / +11.14% | 549.50 / 548.77 / 567.01 | 59.53 / 59.88 / 59.55 | +0.59% / +0.04% |
| 100k-codes-w8-zstd-alloc | 2312.46 / 2319.63 / 2652.79 | +0.31% / +14.72% | 6044.57 / 6038.64 / 6327.17 | 302.02 / 301.50 / 299.47 | -0.17% / -0.84% |
| 100k-codes-w8-none-alloc | 2023.68 / 2051.88 / 2347.48 | +1.39% / +16.00% | 5435.17 / 5496.31 / 5836.68 | 310.01 / 309.28 / 305.41 | -0.23% / -1.48% |
| 32x1025-storage-w8-zstd | 160.41 / 160.97 / 161.52 | +0.35% / +0.69% | 590.90 / 600.58 / 596.03 | 37.96 / 37.30 / 36.88 | -1.75% / -2.86% |
| 1m-storage-w8-zstd | 16559.60 / 16349.84 / 16228.20 | -1.27% / -2.00% | 25230.24 / 24958.89 / 24786.35 | 341.51 / 338.95 / 337.38 | -0.75% / -1.21% |
| 1m-storage-w8-none | 14956.27 / 14952.16 / 14953.87 | -0.03% / -0.02% | 23062.42 / 23013.68 / 23068.53 | 325.93 / 324.31 / 315.15 | -0.50% / -3.31% |
| 1m-storage-w8-zstd-overlay | 15049.50 / 14979.47 / 14943.07 | -0.47% / -0.71% | 23492.24 / 23371.77 / 23306.78 | 339.89 / 338.86 / 337.81 | -0.30% / -0.61% |
| dump-1k-bytecode-w8-zstd | 383.20 / 261.41 / 382.86 | -31.78% / -0.09% | 583.99 / 468.40 / 583.81 | 196.86 / 321.12 / 197.00 | +63.12% / +0.07% |
| dump-1k-bytecode-w8-none | 368.99 / 245.01 / 367.64 | -33.60% / -0.37% | 566.98 / 450.72 / 564.67 | 176.88 / 283.19 / 176.47 | +60.11% / -0.23% |
| dump-10k-w2-zstd | 114.33 / 115.03 / 115.28 | +0.61% / +0.83% | 242.95 / 242.72 / 244.00 | 52.55 / 52.55 / 52.20 | +0.00% / -0.68% |
| dump-10k-w8-zstd | 101.90 / 100.83 / 101.94 | -1.05% / +0.03% | 272.02 / 269.90 / 271.30 | 56.12 / 56.84 / 56.32 | +1.29% / +0.36% |
| dump-10k-w8-none | 95.38 / 95.97 / 96.88 | +0.62% / +1.57% | 264.92 / 269.74 / 268.81 | 42.39 / 43.17 / 42.98 | +1.84% / +1.38% |
| dump-100k-w8-zstd | 1013.35 / 1002.35 / 1019.29 | -1.08% / +0.59% | 3498.86 / 3477.26 / 3501.97 | 208.21 / 210.27 / 208.51 | +0.99% / +0.14% |
| dump-10k-codes-w8-zstd | 139.74 / 123.66 / 139.24 | -11.51% / -0.35% | 414.84 / 396.35 / 414.86 | 67.87 / 68.73 / 67.76 | +1.27% / -0.16% |
| dump-10k-codes-w8-none | 130.80 / 113.69 / 131.00 | -13.09% / +0.15% | 413.99 / 399.56 / 415.34 | 50.78 / 51.41 / 50.41 | +1.23% / -0.72% |
| dump-100k-codes-w8-zstd | 1684.55 / 1447.53 / 1699.45 | -14.07% / +0.88% | 5691.65 / 5473.79 / 5722.45 | 260.93 / 265.41 / 261.30 | +1.72% / +0.14% |
| dump-100k-codes-w8-none | 1589.11 / 1350.51 / 1606.60 | -15.01% / +1.10% | 5594.35 / 5361.20 / 5619.80 | 239.89 / 241.41 / 240.17 | +0.63% / +0.12% |
| dump-32x1025-storage-w8-zstd | 121.06 / 120.31 / 121.50 | -0.62% / +0.37% | 516.99 / 510.43 / 508.94 | 50.58 / 50.99 / 50.10 | +0.82% / -0.94% |
| dump-1m-storage-w8-zstd | 13378.40 / 13341.54 / 13416.06 | -0.28% / +0.28% | 21397.42 / 21307.26 / 21446.85 | 213.47 / 213.15 / 213.55 | -0.15% / +0.04% |
| dump-1m-storage-w8-none | 13457.18 / 13400.83 / 13413.84 | -0.42% / -0.32% | 21527.17 / 21383.63 / 21459.92 | 192.42 / 193.02 / 192.32 | +0.31% / -0.05% |

## Slow samples and maximum reported peaks

All samples remain in paired.json. The following maxima are reported separately from medians; no outliers were discarded.

| Implementation | Slowest CLI sample | Seconds | Maximum peak RSS sample | MiB |
| --- | --- | --- | --- | --- |
| baseline | 1m-storage-w8-zstd pair 3 | 16.677 | 1m-storage-w8-zstd pair 2 | 344.88 |
| current-memory | 1m-storage-w8-zstd pair 0 | 16.934 | 1m-storage-w8-zstd pair 5 | 341.25 |
| current-none | 1m-storage-w8-zstd pair 1 | 16.645 | 1m-storage-w8-zstd pair 5 | 339.25 |

## Allocation fixture

Allocation samples use a separate fixed fixture (10,000 contract storage slots, workers 2, cache 16 MiB, zstd, benchtime 1x), repeated in each CLI phase. They do not represent that phase's account/code/codec dimensions. Cache policies and command match their labels. Every sample is preserved in allocations.txt. Values below are median ms/op / B/op / allocs/op.

| Phase | Baseline | Current memory | Current none |
| --- | --- | --- | --- |
| 1k-bytecode-w8-zstd | 41.347 / 44,189,904 / 245,706.0 | 41.439 / 44,186,320 / 245,705.0 | 40.903 / 43,139,708 / 245,761.0 |
| 1k-bytecode-w8-none | 41.350 / 44,188,680 / 245,682.0 | 40.946 / 44,182,764 / 245,696.5 | 41.389 / 43,137,096 / 245,718.0 |
| 10k-w2-zstd | 40.914 / 44,191,064 / 245,728.5 | 40.880 / 44,186,748 / 245,729.5 | 40.880 / 43,138,468 / 245,756.0 |
| 10k-w8-zstd | 40.905 / 44,185,648 / 245,710.0 | 40.931 / 44,185,260 / 245,683.0 | 40.942 / 43,137,628 / 245,711.5 |
| 10k-w8-none | 40.903 / 44,185,028 / 245,704.5 | 40.872 / 44,187,488 / 245,718.0 | 41.020 / 43,142,712 / 245,718.0 |
| 100k-w8-zstd | 41.451 / 44,188,280 / 245,720.5 | 41.867 / 44,191,664 / 245,687.0 | 40.966 / 43,138,880 / 245,739.5 |
| 10k-codes-w8-zstd | 41.426 / 44,191,956 / 245,730.5 | 41.959 / 44,184,260 / 245,725.5 | 41.449 / 43,137,896 / 245,734.5 |
| 10k-codes-w8-none | 42.844 / 44,189,388 / 245,705.0 | 42.907 / 44,190,240 / 245,687.0 | 42.399 / 43,137,720 / 245,714.0 |
| 100k-codes-w8-zstd | 40.898 / 44,188,616 / 245,678.5 | 41.411 / 44,186,676 / 245,706.0 | 41.865 / 43,132,408 / 245,707.5 |
| 100k-codes-w8-none | 42.368 / 44,188,124 / 245,686.0 | 42.351 / 44,185,864 / 245,706.5 | 41.793 / 43,132,452 / 245,691.0 |
| 10k-codes-w8-zstd-alloc | 44.870 / 44,187,452 / 245,684.5 | 45.291 / 44,191,464 / 245,699.0 | 45.289 / 43,143,372 / 245,717.5 |
| 10k-codes-w8-none-alloc | 46.404 / 44,191,972 / 245,694.0 | 45.434 / 44,192,792 / 245,707.5 | 45.758 / 43,144,572 / 245,717.5 |
| 100k-codes-w8-zstd-alloc | 42.810 / 44,191,388 / 245,718.0 | 43.324 / 44,192,528 / 245,724.5 | 43.836 / 43,132,084 / 245,702.0 |
| 100k-codes-w8-none-alloc | 42.888 / 44,191,524 / 245,718.5 | 41.875 / 44,186,808 / 245,708.5 | 43.319 / 43,136,752 / 245,690.5 |
| 32x1025-storage-w8-zstd | 44.864 / 44,187,556 / 245,671.5 | 43.314 / 44,192,816 / 245,745.0 | 45.804 / 43,138,944 / 245,718.5 |
| 1m-storage-w8-zstd | 41.441 / 44,188,364 / 245,706.5 | 40.822 / 44,192,796 / 245,725.5 | 40.990 / 43,138,624 / 245,749.5 |
| 1m-storage-w8-none | 40.940 / 44,186,764 / 245,719.0 | 42.904 / 44,186,628 / 245,722.0 | 41.131 / 43,140,660 / 245,724.0 |
| 1m-storage-w8-zstd-overlay | 41.867 / 44,184,204 / 245,698.5 | 41.873 / 44,187,560 / 245,714.0 | 41.870 / 43,139,708 / 245,712.5 |
| dump-1k-bytecode-w8-zstd | 28.848 / 38,779,832 / 204,479.5 | 28.960 / 39,841,576 / 204,515.5 | 28.870 / 38,781,628 / 204,478.0 |
| dump-1k-bytecode-w8-none | 29.901 / 38,782,640 / 204,470.5 | 29.915 / 39,842,256 / 204,526.0 | 30.389 / 38,784,152 / 204,495.0 |
| dump-10k-w2-zstd | 28.970 / 38,783,224 / 204,481.5 | 28.929 / 39,833,724 / 204,495.0 | 28.932 / 38,777,984 / 204,466.0 |
| dump-10k-w8-zstd | 28.947 / 38,779,452 / 204,477.5 | 28.942 / 39,840,248 / 204,513.0 | 28.858 / 38,785,032 / 204,492.5 |
| dump-10k-w8-none | 29.874 / 38,781,608 / 204,476.5 | 28.938 / 39,844,708 / 204,516.5 | 29.872 / 38,785,488 / 204,492.0 |
| dump-100k-w8-zstd | 28.935 / 38,782,032 / 204,474.0 | 28.981 / 39,837,596 / 204,507.0 | 29.000 / 38,783,936 / 204,477.5 |
| dump-10k-codes-w8-zstd | 28.930 / 38,783,572 / 204,469.5 | 29.910 / 39,833,936 / 204,492.0 | 29.402 / 38,786,100 / 204,496.5 |
| dump-10k-codes-w8-none | 28.941 / 38,783,604 / 204,476.0 | 30.398 / 39,844,800 / 204,504.0 | 29.405 / 38,782,372 / 204,481.5 |
| dump-100k-codes-w8-zstd | 28.956 / 38,781,088 / 204,471.0 | 28.915 / 39,838,112 / 204,516.0 | 28.883 / 38,784,280 / 204,493.0 |
| dump-100k-codes-w8-none | 29.328 / 38,783,152 / 204,458.0 | 29.930 / 39,841,180 / 204,503.0 | 28.918 / 38,784,092 / 204,456.5 |
| dump-32x1025-storage-w8-zstd | 28.898 / 38,782,844 / 204,491.0 | 29.447 / 39,837,324 / 204,504.5 | 29.809 / 38,780,232 / 204,449.5 |
| dump-1m-storage-w8-zstd | 28.992 / 38,783,532 / 204,487.0 | 28.946 / 39,840,036 / 204,508.0 | 28.949 / 38,786,524 / 204,495.5 |
| dump-1m-storage-w8-none | 28.914 / 38,782,780 / 204,464.0 | 28.967 / 39,833,984 / 204,509.5 | 28.913 / 38,784,164 / 204,490.0 |

## Code-index microbenchmarks

Both sides use the unchanged ordered code index. Nine workloads run six alternating fresh-process baseline/current pairs; raw records include ns/op, B/op, allocs/op and process wall/CPU/RSS. Medians follow.

| Distinct hashes / mode | Baseline ms / B / allocs | Current ms / B / allocs |
| --- | --- | --- |
| 10,000 / shared | 0.642 / 1,978,792 / 99.0 | 0.695 / 1,978,792 / 99.0 |
| 10,000 / overlap | 0.664 / 1,978,792 / 99.0 | 0.664 / 1,978,792 / 99.0 |
| 10,000 / repeated | 1.930 / 1,978,792 / 99.0 | 1.965 / 1,978,792 / 99.0 |
| 100,000 / shared | 6.016 / 16,609,192 / 641.0 | 5.935 / 16,609,192 / 641.0 |
| 100,000 / overlap | 6.473 / 16,609,192 / 641.0 | 6.437 / 16,609,192 / 641.0 |
| 100,000 / repeated | 21.500 / 16,609,192 / 641.0 | 21.593 / 16,609,192 / 641.0 |
| 1,000,000 / shared | 91.633 / 240,976,296 / 9,184.5 | 89.118 / 241,074,664 / 9,187.0 |
| 1,000,000 / overlap | 98.790 / 241,173,032 / 9,191.0 | 98.712 / 241,077,620 / 9,192.5 |
| 1,000,000 / repeated | 565.084 / 241,124,016 / 9,190.5 | 550.187 / 241,173,144 / 9,192.0 |

## Reproduction


```sh
baseline=$(mktemp -d)
# Extract measured baseline identity and reconstruct its sources.
python3 - "$baseline" <<'PYBASE'
import json,subprocess,sys,io,tarfile
from pathlib import Path
identity=json.loads(Path('docs/benchmarks/paired.json').read_text())['baseline_identity']
base=Path(sys.argv[1]).resolve()
archive=subprocess.check_output(['git','archive',identity['head']])
tarfile.open(fileobj=io.BytesIO(archive)).extractall(base,filter='data')
subprocess.run(['git','apply','--binary','-'],cwd=base,input=identity['patch'].encode(),check=True)
(base/'baseline-identity.json').write_text(json.dumps(identity))
PYBASE
(cd "$baseline" && GOFLAGS=-buildvcs=false go build -trimpath -o l2state ./cmd/l2state)
(cd "$baseline" && GOFLAGS=-buildvcs=false go build -trimpath -o verifyfile ./testdata/verifyfile)
(cd "$baseline" && GOFLAGS=-buildvcs=false go test -c -o migration.test ./internal/migration)
PYTHONDONTWRITEBYTECODE=1 python3 scripts/benchmark_suite.py \
  --baseline-binary "$baseline/l2state" \
  --baseline-verifier "$baseline/verifyfile" \
  --baseline-test-binary "$baseline/migration.test" \
  --baseline-identity "$baseline/baseline-identity.json" \
  --baseline-ref btree-lock-task-start-76672b21304c \
  --compare bytes --baseline-index ordered --pairs 6 --out /tmp/l2state-trylock-suite
```

Use a fresh output directory. Individual configurations use scripts/benchmark.py with the same baseline binaries, `--baseline-events --compare bytes --code-cache both` and dimensions from paired.json. Retain/alloc parity uses scripts/benchmark_parity.py with matching CLIs/verifiers.

For metadata boundaries, extract `metadata_boundary.test_source` from parity.json into a temporary Go file. In each checkout create a Go overlay using canonical absolute paths (resolve symlinks), mapping `internal/migration/metadata_review_test.go` to that file, then run `go test -count=1 -overlay <overlay.json> ./internal/migration -run ^TestMetadataReviewBoundary$ -v`. Compare all 64 `BOUNDARY` JSON records by case (artifact hash and complete summary). The test also checks source digests and independently verifies every output.

Replace this README and all three raw files together after a complete campaign. This directory retains only the latest suite; no slow sample or benchmark/test program was removed.
