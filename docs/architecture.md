# Migration architecture

`internal/migration` owns OVM migration and original-state dumping. `statefile` is an independently
importable stream codec and verifier. The root module uses geth v1.17.8; the
separate integration module uses pinned l2geth and never imports the root module.

1. Open source through the strict read-only LevelDB adapter. Validate LastBlock,
   canonical hash, number mapping and exact header RLP. Validate optional audit
   identities using only selected hot canonical/header records.
2. Concurrently traverse/recompute source state and collect source preimages,
   allowance relationships and Transfer-from eligibility from the validated RPC JSON.
   Require JSON progress to match canonical LastBlock and genesis exactly. During
   the validation-only walk, capture accountHash-to-account and per-account
   storageKeyHash-to-nonzero-value B+ trees. Code IDs are reserved in account order.
   Bounded code loaders validate concurrently with storage, including code later
   removed by alloc. Account completion waits for its shared code result without
   holding a worker permit. The ordered consumer bulk-builds the trees from sorted records.
3. Join every validation/index task. Only successful full validation installs the
   memory state and source/head-bound validation evidence. Failed or canceled
   preparation never enables memory reads. Validate retain entries, classify OVM
   slots and holders, then enforce supply/audit accounting before mutation.
   Account and fixed-slot queries use tree searches. Source native balances must
   still be zero. Conversion directly sets holder balances, creates missing
   accounts, deletes converted ERC20 slots and updates OVM_ETH backing/supply/code.
   It creates no account/storage conversion patches. Recompute the changed OVM_ETH
   storage root before freezing; original header/root identity stays separate.
   State transitions are ready -> converting -> frozen -> draining -> consumed;
   errors leave failed state that production output rejects. Failed operations discard memory, not roll back
   source writes: the source is never modified. No output runs during mutation.
4. Exclusively consume account and storage trees in hash order, merging only
   sorted alloc patches, including absent converted accounts. Reuse validated
   unchanged storage roots and the recomputed OVM_ETH root; alloc changes build
   their own accumulators. Preserve pre-alloc roots/counts. With an empty alloc
   map, converted/final account accumulators are shared. Nonempty alloc maps
   retain separate results, including explicit zero fields and no-op objects.
   Alloc does not overwrite converted values. Consumers receive owned scalars.
   Each output is one-shot; concurrent/repeated output on the same state is rejected. The original streaming
   traversal and patch-based conversion remain test-only comparison oracles;
   dump continues its original streaming traversal.
5. Resolve converted/final code references through stable indexes. In memory
   mode, source bytecode comes from the immutable arena; none mode rereads final
   referenced code from the still-open source. Runtime/alloc bytes are input-owned.
   Count inventories separately using validated lengths, without rereading code
   used only before alloc. Emit source codes in first-reference order, then new
   runtime code, then new alloc code ordered by keccak(address). Writer independently validates the
   stream, code and final roots. Sync and close output, close source, check
   cancellation, then atomically publish without replacement and sync the parent.

## Original-state dump

`Dump` has separate options containing only source/output and resource settings.
It opens the same read-only legacy database, captures LastBlock/header and the
hot canonical genesis hash. It creates no
OVM balance-slot, allowance or membership maps, and does not seed the OVM_ETH
address or read event/audit/alloc/runtime inputs.

A single raw-source walk validates accounts, storage, code and addresses while
emitting original state into an unpublished temporary file. After bounded account
admission, each account worker performs exactly one source Get for
`secure-key- || accountHash`, requires a 20-byte address and verifies its Keccak
hash against the account trie key. Missing, empty, wrong-length or mismatched
preimages and database read errors fail closed. No namespace scan, address map,
external fallback or validation proof is used. Unreferenced preimages (including
malformed records) are not inspected. The address lives only in the active account
and its bounded output queue. The global limiter covers the point read and is
released before emitting records or waiting on queues. Dump reuses one LevelDB
Snapshot for these address queries, avoiding per-query acquisition/release of the
global snapshot reference. This handle pins only a read sequence; it does not
copy state or retain addresses. It is released after workers join and before the
source database closes, including failure/cancellation paths. Trie and code reads
retain their ordinary DB path. Migration does not acquire this handle.

Native balances are accepted. The walk reconstructs source storage/account roots,
while Writer independently reconstructs output roots. No conversion or alloc
patches are applied, and the ordered bounded queues are unchanged. Code is resolved
from the source without overrides and deduplicated. Large storage still uses its
bounded threshold probe before partition traversal, and code is hash-checked during
traversal and, in the default `--code-cache=none` mode, read again for output.
`--code-cache=memory` retains deduplicated bytecode for output. Single-pass therefore does not mean reading
every database key exactly once. Errors discovered after writing begins discard
the temporary file; only a fully validated artifact can be published. Migration
retains its address/event indexes, snapshot preparation and zero-native-balance
restriction.

Writer enforces dump semantics (`IndexVersion=0`): no event provenance, identical
source/converted/final roots and inventories, and zero conversion statistics.
The source header RLP and hashed storage keys remain unchanged. Sync, close,
source close, cancellation and atomic no-replace
publication follow the same sequence and error contract as migration. Neither
pipeline rereads its output. Tests independently verify artifacts and rebuild
state using the separately pinned legacy module.

## Concurrency and memory

Account and large-storage traversals split the first trie nibble into 16 ranges.
Every source iterator is goroutine-exclusive. The shared worker limiter covers
iterator advancement and index work, and is released before sending or waiting.
An ordered account admission window permits at most `2 * workers` in-flight
accounts. The earliest account is always admitted before later accounts.

Each partition queue has 64 bounded leaves (account RLP at most 110 bytes; storage
words at most 32 bytes after decoding), and each account output queue has 64
fixed-size state events. Channel elements remain two pointers; queued records
omit the public code/footer fields, reducing payload size without changing the
64-record cap or the `2 * workers` admission window. Code records never enter
these queues. Storage up to
1024 slots uses a bounded probe; the 1025th discards the probe and starts ranged
iteration before emitting anything. Probe words are stored as fixed-size hashes,
without a separately allocated slice per value. Migration retains full storage in the snapshot; the queues and temporary
probe remain bounded. Dump retains no full-state snapshot.

Fixed record encoding/decoding reuses per-reader/writer scratch. Returned records
still own their contents, including short code and balances. Storage trie RLP is
encoded into a local 33-byte buffer; geth's StackTrie copies the value during
Update. Compact scalar records omit integer padding and default hashes; the reader
restores full values before validation. Nonempty storage roots remain explicit.
Canonical variable-length record headers are included verbatim in the checksum.

Migration uses Writer's default encoder ownership, with no readback scratch
handoff. Close/abort clear codec references and block buffers. The statefile
package's optional operation-local cache remains available to internal callers
that use it; compression options and semantic validation are unchanged. There is no global cache or forced GC.

Migration uses an account B+ tree and one storage B+ tree per account. Leaves
store inline hash/value entries (account values are pointers to owned accounts);
branches hold at most 32 children and their upper bounds. Leaf capacity grows
from one entry to at most 128, avoiding a full leaf allocation for tiny storage.
Sorted capture appends along the right spine and fills completed nodes; it never
materializes the complete key set. Conversion supports lookup, replacement,
insertion/splitting and deletion. Empty nodes are pruned and single-child roots
collapse; sparse nonempty nodes are not rebalanced, so space after deletions can
reflect historical occupancy. Leaves remain at one depth. There are no tombstones,
copy-on-write versions, per-slot objects or node pools. Source roots are cached,
and only OVM_ETH's changed storage root is recomputed before output.

An account owns nonce and optional nonzero balance, immutable code-hash reference,
nonempty root and storage tree. Nil defaults preserve empty fields; no-code
accounts may still own storage. Readers return owned scalar values. After source
validation/accounting and conversion join, output acquires exclusive ownership.
The account scheduler drains leaves and passes each account directly to its worker;
workers never query the account tree concurrently with its destruction. Admission
remains bounded to `2 * workers` accounts, each with a 64-event queue.

Storage is drained in leaves of at most 128 entries. After processing each leaf,
the worker queues a barrier behind all emitted records and waits without holding
a worker permit. The ordered consumer acknowledges the barrier after successful
Writer calls; only then is the leaf cleared and detached. Even an alloc deleting
all entries must acknowledge each batch. Account-leaf barriers wait for the last
ordered account stream, including AccountEnd, both root/count accumulators, code
references and worker completion. This bounds retained completed account batches.
No output path repeatedly calls DeleteMin or rebalances a tree being consumed.

Writer acceptance is not fsync: encoded bytes may remain in compression buffers.
Later failures discard the unpublished artifact; consumed state is never retried.
Cancellation wakes barrier waits, propagates the first error, joins workers and
drops remaining state. Output checks cancellation before admission and uses
`TryLock` to acquire exclusive snapshot ownership. Concurrent output is rejected
immediately, even while the active Writer is blocked; already-canceled calls
return their context error. Rejected calls neither consume state nor interrupt
the owner. Later calls are rejected as consumed/failed. Shared code remains available for
the later code section. Releasing references makes nodes GC-eligible, not an
immediate RSS reduction. No forced GC or unbounded node freelist is used. Full
validation/conversion still needs the entire state, with no hard RSS bound.

The immutable code directory still uses fixed blocks of 48-byte records and code
bytes occupy 1 MiB blocks. A code-hash lookup map deduplicates code and supplies
stable IDs; IDs never enter file bytes. Code readers return owned copies,
including code spanning block boundaries. Capture synchronizes directory lookup
with loaders. Conversion replaces only the OVM_ETH code reference, never shared
source code. Output code order is independent of account tree layout.
The code directory uses blocks of 1,024 records. A fixed pool of `max(1, workers / 2)` code readers consumes a queue of at most
`2 * workers` tasks. Only active loads retain result channels, bounded by the
account admission window; repeated hashes share one
in-flight result. The permit covers the DB read/hash, not queue or child waits.
Bytecode completion order can change arena offsets, never stable IDs or file bytes.
Memory caching defaults on for migrate and off for dump. None mode retains the
same validated directory but allocates no bytecode arena. Transient code buffers
are bounded by admitted accounts and the 128 MiB per-code limit.

Additional migration maps hold address/slot ownership, eligibility, temporary
balance analysis jobs and sparse alloc patches. Both commands retain source-validation code deduplication; dump has no
address index. Each output walk merges converted/final reference flags through
a codeHash-to-index map and fixed blocks of 1,024 reference records. Collection
is average O(1) per reference; this code index uses no B-tree or code-hash sort.
Writer retains its independent reference-validation map, rejecting duplicate
(including nonadjacent), missing or orphan code. There is no scratch database.
geth's transitive module graph may include database libraries; their presence in
go.sum does not imply opening or creating those databases.

Cancellation preserves the original error, cancels siblings and joins every job
before closing the source. Migration allocation/RSS depend on the complete
snapshot as well as index and patch sizes; bounded queues are not a process-memory bound. There is no automatic
spill, fallback or hard memory budget. Source, converted and final full-state
copies are never held together; OVM conversion mutates the single memory state,
then alloc is merged while emitting final records.

## Validation boundaries

The original state root, every source storage root and all source code are checked
before any policy changes, even when alloc would replace them. Migration's
memory output reuses unchanged storage checks and the recomputed OVM_ETH root.
Original source identity is retained separately under the stopped-source requirement.
Dump validates during its single streaming walk. Writer still independently hashes
all final storage/account records and validates code, roots and inventories while
writing. Migration does not
reopen the file; independent verification is performed by tests and benchmark
acceptance outside measured migration time. Consumers can use the public Reader
or Verify to check final contents without the source. A validated checksum proves
byte integrity, not the authenticity of an external audit or node event history;
the reader cannot independently replay OVM policy from provenance alone.

Inputs and their digests are captured during initial parsing, with no final
path-based reread. JSON is decoded with bounded tokens and hashed once; its
relationship sequences use the v1 length-framed IndexDigest encoding in saved
array order. The digest excludes JSON formatting but binds record order. Preparation
binds event evidence to source genesis and canonical head/header without reading
retired node index markers. After successful capture, migration does not reread
source account/storage state. Memory mode also avoids source code rereads;
none mode revalidates each final source code during output and propagates read
errors. Source close errors are still propagated before publication. These checks do not replace the stopped
database or consistent-snapshot requirement.

`internal/ovmevents` owns strict checkpoint parsing, event selection, RPC polling
and atomic checkpoint replacement. Checkpoints contain chain identity, progress
and relationships; protocol constants stay in code and allowance trie keys are
derived from owner/spender. Members are a unique address sequence without per-member
positions; each still originates from a validated nonzero Transfer log. The
set's canonical boundary is the checkpoint genesis and progress. One loop owns
its relationship maps and slices. Logs are ordered by block number and log index;
batch-local maps deduplicate and slices retain first occurrences. Only after full
batch validation and the reorg check are those slices appended to the checkpoint
and progress advanced. Network retries preserve that prefix, while
reorgs fail closed. The checkpoint writer streams records in saved order and uses an
exclusive output lock. Migration never invokes RPC or starts synchronization.
Readers reject duplicates using maps; the absence of per-record event positions
means first-occurrence order is a synchronizer guarantee, not a reader check.

The frozen file corpus checks the current compact v1 encoding. Code-hash sorting
was removed in place: the format stays at v1 without backward compatibility. Legacy
integration compares RPC-derived relationships with node-generated indexes,
removes the fixture node indexes, checks migration without ancient,
then reconstructs the output with l2geth trie/state APIs. Synthetic receipts test
index interoperability; separate wrappedEther execution/continuation tests cover
the converted runtime. Neither is production-chain execution evidence.
