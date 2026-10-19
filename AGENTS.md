# Repository guidance

This repository exports the latest executed legacy state into one reusable binary
file. `l2state` has `migrate`, `dump`, `help` and `version` commands. `migrate`
applies OVM conversion; `dump` preserves original state without OVM inputs.
The separate `ovm-event-sync` executable builds resumable RPC event checkpoints.

- Never write, repair, recover or compact the source. Require a stopped database
  or consistent snapshot. Migration never reads ancient/freezer or receipt history;
  the sync tool obtains historical events only through standard log RPC.
- For migrate, require --ovm-events RPC JSON bound exactly to canonical LastBlock
  and genesis.
  Missing, corrupt or ambiguous evidence fails closed; no node-index or witness
  fallback. State, code, address preimages and audit evidence remain read-only DB
  inputs. RPC log completeness is trusted; reorgs stop the sync tool.
- Preserve source header RLP. Its root is the original root, **not** finalStateRoot.
  StorageKeyHash is already hashed. Never hash it again in a trie consumer.
- Migrate validates and captures full state as accountHash-to-account and
  storageKeyHash-to-value B+ trees using ordered bulk construction. After source validation and accounting succeed,
  OVM conversion mutates memory directly without conversion patches. Output then
  exclusively consumes trees in order, releasing storage leaves after Writer
  acknowledgement and account leaves after their workers/accumulators finish.
  Reject concurrent output immediately with TryLock; canceled admission and
  rejected contenders must not consume state or interrupt the active owner.
  Never replay consumed state or retain released nodes in a freelist. Alloc remains a post-conversion sparse overlay. Source
  bytecode is optional: --code-cache=memory retains it, none rereads final referenced code;
  defaults are memory for migrate and none for dump. Dump keeps streaming state.
  Reserve code IDs in source-account order before parallel code/storage work.
  New runtime code follows source codes; alloc appends new code by ascending
  keccak(address). Replace account references, never shared code contents.
  Use bounded queues and fixed-size bytecode blocks; never scratch databases.
  Account/storage trees grow with state cardinality; no full-state sorting slice.
  No worker permit while waiting on a queue or child.
- Dump uses only source address preimages, accepts native balances, and never
  applies OVM conversion or alloc. IndexVersion=0 means a strict original-state
  dump: no event provenance, equal source/converted/final roots and inventories,
  and zero conversion statistics. Existing IndexVersion=1 migration is unchanged.
- For migrate, finish source validation and conversion before alloc. Preserve exact auditing,
  retention and sparse overlay semantics described in README.md.
- Validate through Writer, sync and close the output, then close the source
  before atomic no-replace publication. Do not reread
  output or rehash input/index contents during migration. Independently verify
  artifacts in tests and outside benchmark timing. Propagate close/sync errors;
  retain a published file on parent-sync failure.
- Keep stdout JSON separate from stderr progress. No timestamps/paths in file bytes.
- Keep statefile at v1 during active development; do not bump the format version.
  Approved format changes apply directly to v1 without backward compatibility.
  Do not add legacy decoders, compatibility modes or format converters. Keep the
  documented format and frozen corpus aligned with the current implementation;
  review intentional fixture changes explicitly, never silently regenerate them.
- Preserve Go 1.27.2, geth v1.17.8 and the independently pinned legacy fixture module
  unless dependency changes are specifically in scope.

Finish with `make ci`, `make test-race`, and `git diff --check`. For concurrency or
performance changes, retain cancellation/ordering/bounds tests and run the paired
fresh-process benchmark harness. Report synthetic, local, remote-CI and production
evidence separately. Do not modify the reference migration repository.

After each completed performance test, consolidate the latest full benchmark
suite into `docs/benchmarks/README.md` and replace `paired.json`, `allocations.txt`
and `parity.json` with its raw results. Keep only that latest suite; remove
superseded reports, raw results and diagnostic artifacts, and update all links.
Preserve the latest baseline identity, environment, methodology, all samples
(including slow samples), correctness evidence and reproduction instructions.
Do not mix results from different runs or remove benchmark/test code.
