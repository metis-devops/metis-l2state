# metis-l2state

Export a stopped legacy Metis l2geth state into one reusable binary file.
`migrate` converts OVM state using hot LevelDB and a required RPC-derived event
JSON file; `dump` exports the original state using only hot LevelDB. Neither
command creates a target database or temporary database. The separate `ovm-event-sync`
tool builds and resumes the event file using standard Ethereum JSON-RPC.

```sh
make build
bin/l2state migrate \
  --source-chaindata /snapshot/geth/chaindata \
  --out /exports/state.l2state \
  --ovm-events /exports/events.json \
  --wrapped-ether-code ./wrapped-runtime.hex \
  --workers 8
```

The runtime is an explicit operator-supplied, storage-compatible wrappedEther
runtime, encoded as nonempty `0x` hex. The test runtime is not a production default.
The output parent must exist; the output must be new and outside the source.
The source must be stopped or a consistent filesystem snapshot.

## Original-state dump

```sh
bin/l2state dump \
  --source-chaindata /snapshot/geth/chaindata \
  --out /exports/original.l2state \
  --workers 8
```

`dump` exports the canonical LastBlock state without changes: native balances,
nonces, code and storage are preserved, including empty accounts present in the
source. It does not require OVM_ETH or perform any token, supply, allowance or
retention checks. All OVM options, including event JSON, runtime, audit, retain
list and genesis alloc, are rejected. It does not read RPC or node event indexes.

Both commands support `--compression zstd|none`, `--workers`, `--cache-mb`,
`--handles`, `--code-cache memory|none` and `--quiet` with the defaults below. They share the stopped-source,
read-only hot LevelDB and atomic no-replace publication requirements. Missing
or corrupt preimages for exported accounts (including OVM_ETH), code or trie
state fail closed.
Only the current LastBlock is supported; this is not a modern geth database or
historical-block exporter.

Dump files use the same v1 encoding with `Metadata.IndexVersion = 0`. The three
summary roots all equal the source header root; both inventories are identical.
Conversion amounts are `"0"` and conversion counts/audit fields are zero because
no conversion was performed; these are not measurements of source token balances.
Genesis identity is preserved, while event-index fields are zero and input hashes
are absent. The Reader supports the current v1 dump and migration formats. During active
development, format changes apply directly to v1 without backward compatibility.

## State and file semantics

- `header` preserves the selected LastBlock header byte-for-byte. **header.Root
  is the original state root.** `Summary.FinalStateRoot` describes output state.
  No synthetic checkpoint or bootable chaindata is produced.
- Accounts carry real addresses, uint256 balances, uint64 nonces and code hashes.
  Missing account-address preimages fail. Accounts are ordered by keccak(address).
- Storage carries `StorageKeyHash = keccak256(original slot)` and nonzero 32-byte
  values. Consumers rebuilding tries use this key directly; it is not a SetState slot.
- Code is deduplicated through a codeHash-to-index map and emitted after accounts.
  Source codes keep first-reference order in the source account traversal; new
  conversion runtime follows, then new alloc code in ascending keccak(address)
  order. Existing hashes reuse their indexes; unused codes are omitted.
- Migration files include index provenance, exact input SHA-256 hashes, conversion
  amounts and pre-alloc/final roots. Both commands include source identity,
  inventories and a stream checksum; dump has no conversion or event provenance.
- Compression is `--compression zstd|none` (default zstd). Both use bounded blocks.
  Identical inputs, tool version and compression give identical bytes across workers.

See [the compact v1 format](docs/format.md) and [architecture](docs/architecture.md).
The format stays at v1; code records no longer require code-hash ordering. No
legacy decoder or compatibility mode is provided. Integers use minimal length-prefixed bytes;
empty code hashes and empty storage roots are implicit. The public Go reader
restores full values through the same streaming API.

## Migration memory

`migrate` reads and fully validates the source state while building
`accountHash -> account` and per-account `storageKeyHash -> nonzero value` B+ trees.
Sorted capture bulk-builds full leaves from left to right without sorting arrays.
Only after source validation and event index loading succeed does conversion
begin. It classifies balances and verifies supply/audit accounting before any
mutation, then directly sets native balances, creates missing holder accounts,
deletes migrated ERC20 slots and updates OVM_ETH supply/backing/code. No OVM
conversion diff is created. Failed conversion is not eligible for output.
After conversion, output takes exclusive ownership and consumes the trees in
hash order, applying only the optional alloc overlay. Each storage leaf contains
at most 128 entries and is released after the Writer acknowledges all resulting
records. Account leaves are released after their bounded worker streams and
root/count/code-reference accumulation complete. Output cannot be replayed or
run concurrently on the same state. Concurrent output is rejected immediately,
without waiting for the active Writer; an already-canceled call returns its
context error before acquiring ownership. Rejected calls do not consume state
or interrupt its owner. Once output starts, failures require a fresh migration.
Conversion and output never traverse the source state trie again.
`--code-cache=memory` retains unique source bytecode in fixed-size blocks;
`--code-cache=none` validates it, retains only its hash/length/index, and rereads
final referenced code for output. Migrate defaults to `memory`; dump defaults to
`none`. Both modes produce identical bytes. Runtime/alloc code remains owned by
the parsed inputs in either mode. Code reads overlap account storage work, with
stable IDs assigned before dispatch and bounded in-flight loads.
`dump` validates and writes in one source-state traversal. For each admitted
account it reads `secure-key- || accountHash` once through a reused LevelDB read
sequence handle, requires exactly 20 bytes and
checks `keccak256(address) == accountHash`. It neither scans the preimage namespace
nor retains an address map; unrelated preimages are not inspected. It does not
capture a full-state snapshot; Writer independently checks
the emitted roots before atomic publication. A late source error discards the
unpublished partial file.

Zero balances, empty code and empty storage roots use implicit defaults; empty
storage trees have nil roots. Nonempty code hashes reference the immutable shared code
directory. Nonempty storage roots are cached after validation; conversion
recomputes the changed OVM_ETH root before freezing. Native balances are allocated
only when nonzero. The original header/root remain separate source identity.

Tree nodes, per-account objects and optional fields add memory and GC overhead.
Branches have at most 32 children; leaf entries are inline and tiny storage trees
grow from one entry. Conversion inserts split full nodes and deletes prune empty
nodes; sparse nonempty leaves are not rebalanced. There is no full-state key sort,
copy-on-write clone, node freelist or per-slot pointer allocation. Consumed nodes
become eligible for GC; RSS need not fall immediately, and output release cannot
undo a peak already reached during validation or conversion. Source-code directory records remain
48 bytes and cached bytecode uses fixed blocks. Address/event indexes, temporary
conversion jobs, alloc, LevelDB cache, Writer and runtime overhead are additional.
There is no fixed bytes-per-account/slot or RSS guarantee, automatic spill,
streaming fallback or process memory cap. Provision memory for the full state;
see the [measured comparison](docs/benchmarks/README.md). There is no scratch DB
or second complete converted/final state copy. Alloc is merged during output and
does not overwrite converted values; converted roots/counts remain separate.

The original source root, every storage root and all source code remain fully
validated before conversion, including data later replaced by alloc. Writer
independently validates final records and roots. The source stays open until
output sync/close, and publication ordering is unchanged.

## Conversion and inputs

All source native balances must be zero. EOA balances convert to native balances.
Contracts retain ERC20 balances if the event JSON records a nonzero OVM_ETH Transfer
whose `from` is that contract, or if explicitly listed in the retain list.
This includes transferFrom/burn events; zero-value Transfers never grant eligibility.
OVM_ETH self holdings remain. Its new native backing and ERC20 totalSupply both
equal the retained balance sum. Allowances and recognized metadata are preserved.
Short OVM name/symbol strings use only the fixed metadata slots. Long strings
retain the original uint64 length domain and use cancellable, lazy slot-hash
matching instead of an expanded key index. Metadata workspace is fixed; this
does not bound search time for unusually long strings. Raw dump is unchanged.

| Option | Meaning |
| --- | --- |
| `--ovm-events FILE` | Required RPC event checkpoint with genesis and progress matching source LastBlock exactly. |
| `--ovm-erc20-retain-list FILE` | One `0x` address per line. Every address must be an existing non-OVM_ETH source contract, even when its balance is zero. Duplicates and blank/comment lines fail. |
| `--ovm-genesis-alloc FILE` | Post-conversion GenesisAlloc map. Omitted fields remain unchanged; explicit zero/empty scalars apply; storage merges, with zero deleting a slot. Unknown/duplicate/null fields fail. OVM_ETH cannot be overridden. |
| `--ovm-audit-summary FILE` | Exact mvm v1 audit summary permitting only a fully explained supply discrepancy. No arbitrary holder debit or tolerance. |
| `--workers N` | Shared source work allowance, 2–16. Default CLI value is CPU count clamped to this range. |
| `--code-cache memory\|none` | Retain all unique source bytecode (`memory`) or reread final code (`none`). Defaults: migrate `memory`, dump `none`. No automatic spill. |
| `--cache-mb N`, `--handles N` | Source LevelDB allowances, defaults 512 MiB / 256. Cache is not an RSS limit. |
| `--quiet` | Suppress stderr progress; stdout remains the final JSON result. |

Retain-list and alloc inputs cannot supply missing source address preimages.
Alloc cannot fix invalid source state or change eligibility. An empty account or
storage object is a no-op; explicit scalar fields or storage entries can create
an otherwise empty account. Explicit native balance overrides may change totals.

The migration does not read node allowance/member indexes or their
meta/head/ready/disabled markers. Missing or invalid JSON fails without a fallback.
State, code, address preimages and audit evidence still come from the stopped hot
database. Event relations supplement known addresses; they do not reconstruct all
address preimages. Genesis/direct-storage allowances lacking Approval evidence
still fail when needed for slot classification.

## RPC event synchronization

```sh
# Follow the chain indefinitely; the same command resumes an existing checkpoint.
bin/ovm-event-sync --rpc-url https://your-node --out /exports/events.json

# Use the stopped snapshot's exact LastBlock height; wait if RPC is behind.
bin/ovm-event-sync --rpc-url https://your-node --out /exports/events.json \
  --to-block 123456 --batch-size 1000 --poll-interval 5s \
  --flush-interval 30s --rpc-timeout 30s
```

Both executables support `help` and `version`. Sync accepts `--quiet`, sends
progress/retries to stderr and emits a final JSON result to stdout after reaching
its target. Without a target it continues polling. SIGINT/SIGTERM saves the last
fully validated batch and returns cancellation (nonzero exit). A forced kill may
lose unflushed work; restart replays it from the last checkpoint.

The RPC must provide headers and complete historical `eth_getLogs` results from
block 0. The tool validates each header's hash, height and parent, every event's
position and encoding, and the batch endpoint before advancing. All Approvals
(including zero amount) contribute owner/spender relationships. Only nonzero
Transfers contribute `from` membership, including burns and transferFrom.
No current allowance or token balance values are fetched.

`--batch-size` bounds a batch to 1–10000 blocks (default 1000). Headers are checked
sequentially; event queries cover the batch. Range-limit responses halve the
range; a failing single-block range stops. Transient network errors, rate limits
and timeouts retry with 1–30 second backoff. Unsupported methods, malformed data
and identity mismatches stop. The loop has one owner and no worker pool.

The v1 checkpoint has exactly six fields: `version` (1), `chainId` (decimal
string), `genesis`, `progress` (`blockNumber`, `blockHash`), `allowances`
(an array of two-address arrays in fixed `[owner, spender]` order) and
`members` (an array of address strings such as
`["0x0000000000000000000000000000000000000001"]`). Members identify senders of
nonzero Transfers anywhere in the processed prefix; no per-member block position
is stored. The RPC source, OVM_ETH contract, Transfer/Approval topics and allowance mapping slot 1 are fixed by the implementation, not input
fields. The secure storage trie key is derived from each owner/spender pair;
it is never supplied by the checkpoint.

Allowances are sorted and unique by that derived key; members are sorted and
unique by address. Genesis and progress bind the entire set to one canonical
prefix. Synchronization still validates each log against its canonical block
header before adding its sender to the set, including repeated senders.
Readers reject duplicate, missing, unknown or null fields, including the removed
`source`, `contract`, `allowanceSlot`, `transferTopic`, `approvalTopic` and
allowance `storageKeyHash` fields. Each allowance must contain exactly two
address strings; missing or extra elements, nulls and the former
`{"owner": "...", "spender": "..."}` objects are rejected.
Strings must contain valid UTF-8. Address and hash strings use geth's
fixed-width, hex-prefixed JSON decoding; JSON escapes inside them are rejected.
Member objects, including the former
`address`/`blockNumber`/`blockHash` form, are rejected. The format remains v1 with
no legacy decoder or conversion mode; checkpoints from the earlier schema must
be rebuilt from RPC into a new file. JSON tokens are bounded while streaming; relationship
indexes grow with cardinality, not full storage state.

An existing file resumes only on the same chain and canonical checkpoint hash.
A lower target is rejected; an equal target is checked without replay. Any
observed reorg stops and requires a new output file rebuilt from genesis.
The JSON is trusted operator input based on RPC log completeness, not a receipt
root proof. Neither a matching checkpoint nor a checksum proves omitted logs
were returned by the provider.

The output parent must exist. A persistent `.lock` sibling prevents cooperating
writers from sharing an output; do not delete it while a process is running.
Each refresh streams to a same-directory temporary file, syncs and closes it,
atomically replaces the checkpoint, then syncs the directory. Pre-rename failures
keep the previous checkpoint. A directory-sync failure retains the new checkpoint
and reports uncertain durability. Orphan `.ovm-events.partial-*` files are never
used for recovery. Refreshes occur at request/batch boundaries, so a slow RPC
request or large checkpoint write can delay the configured interval.

For migration, JSON progress must exactly equal the stopped source LastBlock
height and hash, and genesis must match. A checkpoint ahead of the snapshot is
rejected; generate a separate file with `--to-block` for that snapshot.

Without audit input, balances must sum exactly to totalSupply. With audit input,
actual balances must equal migrated plus retained amounts. For historical audit
heads, current code hash and net discrepancy must match; for the current head,
slot counts and amounts must match too. Genesis and audit-head canonical mappings
and headers must remain in hot LevelDB; missing evidence fails without opening
ancient. Historical audit completion does not prove later offsetting anomalies absent.

## Docker

Build the standalone synchronizer image with its own default entrypoint:

```sh
docker build --target ovm-event-sync -t ovm-event-sync:local .
docker run --rm ovm-event-sync:local version
mkdir -p ./event-data

# Follow head; reuse this directory and output name to resume after restart.
docker run -d --name ovm-event-sync \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,source=$(pwd)/event-data,target=/data" \
  ovm-event-sync:local \
  --rpc-url https://your-node --out /data/events.json

docker logs -f ovm-event-sync
# Allow time for the final checkpoint write on SIGTERM.
docker stop --timeout 60 ovm-event-sync
docker start ovm-event-sync
```

Mount the entire writable output directory: checkpoint replacement, its lock and
temporary files must share that directory. Mounting only `events.json` does not
support atomic replacement. The directory persists when the container is removed.
Increase the stop timeout if writing a large checkpoint takes longer; a forced
kill resumes from the last completed checkpoint. Logs/progress remain on stderr.
The RPC endpoint must be reachable from inside the container.

For a one-shot target, first stop any container using the same checkpoint, then:

```sh
docker run --rm \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,source=$(pwd)/event-data,target=/data" \
  ovm-event-sync:local \
  --rpc-url https://your-node --out /data/events.json --to-block 123456
```

Completion prints the result JSON and exits. RPC history and reorg checks are the
same as the native executable. The migration image remains the default build:
`docker build -t l2state:local .`; it also retains both binaries, so existing
`--entrypoint ovm-event-sync` invocations continue to work.

## Public Go reader

```go
f, err := os.Open("state.l2state")
if err != nil { return err }
defer f.Close()
r, err := statefile.NewReader(ctx, f)
if err != nil { return err }
defer r.Close()
for {
    record, err := r.Next()
    if errors.Is(err, io.EOF) { break }
    if err != nil { return err }
    // Consume AccountStart, StorageEntry, AccountEnd, CodeEntry, Footer.
    _ = record
}
summary, err := r.Result() // available only after complete verification
```

Import `github.com/metis-devops/metis-l2state/statefile`. Reader/Writer do not own
the supplied stream. `statefile.Verify` consumes and validates the entire file.
An early reader close validates only the prefix; do not publish consumer side
effects before complete validation. The reader verifies the file's state/root,
not a replay of the source-chain conversion policy.

## Operations and validation

Both commands validate source state; migration additionally validates conversion.
Writer checks output records,
roots, code and inventories while writing. After syncing and closing the file,
it closes the source, then publishes atomically. Source genesis and head/header
are validated initially and are not rechecked before publication. Success does not include independent file readback;
consumers can use `statefile.Verify` for that check.

For migration, input data and SHA-256 digests describe the bytes parsed initially. Inputs are not
reopened before publication. The `ovm-events` input digest covers the original JSON
bytes, and IndexDigest covers its normalized v1 relationships and identity.
Replacing a JSON path later does not alter the captured input. A stopped source
or consistent snapshot remains required.

Account/storage trees, identity/eligibility indexes and alloc grow with input
cardinality. Source ingestion and output use bounded queues and acknowledged
leaf batches; alloc and conversion-job ordering still use temporary key slices.
Memory is monitored, without an
internal limit or disk fallback. Resource limits belong to the process/container.
Ordinary failures clean the invocation's partial file. A forced process kill can
leave a `.partial-*` sibling; it is never a published artifact. A parent-sync error
after rename retains the final file and returns `PublicationDurabilityError`.

```sh
make ci
make test-race
python3 scripts/benchmark.py --baseline-root /path/to/metis-l2geth-migration \
  --accounts 10000 --pairs 6 --out /new/benchmark-directory
```

CI includes lint, unit/race/fuzz checks, a separately pinned l2geth interoperability
module, amd64/arm64 builds and Docker smoke tests for both entrypoints. Main/tag
publication builds separate multi-platform images at
`ghcr.io/metis-devops/metis-l2state/l2state` and
`ghcr.io/metis-devops/metis-l2state/ovm-event-sync`, with SBOM/provenance. Use the
corresponding published tag in place of the local image names above. Local tests do not
establish remote CI, published images or production-snapshot acceptance.

For operator-provided original-state dumps, the test-only restorer creates a
new, disposable legacy LevelDB containing current trie nodes, bare-hash bytecode,
preimages, genesis/head headers and head mappings. It does not restore historical
blocks, receipts, genesis state, or a runnable full node. Inputs are read-only;
an existing destination is rejected and ordinary failures remove the incomplete
new database. Storage keys are inserted as already-hashed keys. The optional
`--target` compares the complete target header RLP with the dump's embedded RLP.

```sh
go run ./testdata/restorestate --dump fixtures/dump.zstd \
  --preimages fixtures/preimages.rlp.gz --genesis fixtures/genesis-header.json \
  --target fixtures/target-block-header.json --out /new/restored-chaindata
(cd testdata/legacyintegration && \
  L2STATE_RESTORED_SOURCE=/new/restored-chaindata \
  go test -count=1 -v -run '^TestRestoredLegacyState$')
```

`testdata/dumpprofile` accepts `--command migrate`, `--ovm-events`, `--audit`
and `--runtime` to profile a migration; CPU, lock and allocation profiles are
collected outside timing. `--mode all` collects them together with configurable
sampling intervals. `scripts/benchmark_mainnet.py` compares two CLI binaries in alternating
fresh processes against one stopped restored fixture, verifies every output
outside timing, compares exact bytes and complete summaries, and checks source
and input immutability. Reconstruction layout and local cache state differ from
the original node, so these measurements do not establish production performance.

The [latest benchmark report](docs/benchmarks/README.md) includes paired timings,
CPU, peak RSS, allocation measurements and independent artifact verification.
Only the latest completed benchmark suite and its raw results are retained.
