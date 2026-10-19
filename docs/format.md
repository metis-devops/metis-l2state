# Binary format v1

The format remains v1 during active development: the magic ends in byte `01`.
Approved format changes apply directly to v1 without backward compatibility;
no legacy decoder, compatibility mode or converter is provided. Code records
no longer need to increase by hash. Compact payloads, compression framing and
metadata/index versions otherwise remain unchanged.

Fixed-width lengths and compact scalar payloads are big endian. Record payload
lengths use unsigned LEB128 as specified below. Hashes and addresses are raw
32/20-byte values. RLP structs follow the exported Go field order in `statefile`.

## Preamble

| Bytes | Meaning |
| --- | --- |
| 8 | `L2OVMST` followed by byte `01` (format version) |
| 1 | Codec: `00` none, `01` zstd |
| 4 | RLP header payload length, 1..1 MiB |
| variable | RLP `[sourceHeaderRLP, Metadata]` |

Source header bytes are preserved exactly. Metadata is ordered as ToolVersion,
IndexVersion, Genesis, IndexHeadNumber, IndexHeadHash, IndexDigest, AllowanceCount,
MemberCount, Inputs. Inputs is an ordered list of `[Name, SHA256]` entries, sorted
by name; migration uses `alloc`, `audit`, `ovm-events`, `retain`, `runtime` when present.
`ovm-events` is the SHA-256 of the exact initial RPC checkpoint JSON bytes.
Index digests hash length-framed key/value pairs in metadata/head/ready,
allowance-key order, then member-address order. Each length is uint64 BE. For
RPC event inputs these records are reconstructed from checkpoint identity,
fixed protocol constants and sorted relationships. Synthetic head/ready
positions both equal checkpoint progress. Allowance trie keys are derived from
owner/spender at mapping slot 1.

Each member record has key `ovm-transfer-v1/member/` followed by its 20 address
bytes, and an empty value (a zero uint64 value-length field, with no value bytes).
No member block number or block hash enters the digest. Removing per-member
positions intentionally changes the normalized index digest, while the format
and IndexVersion remain 1. Checkpoint JSON changes also change the raw input
SHA-256. This encoding does not assert node index readiness or prove historical
completeness.
Input provenance is distinguished by `ovm-events`; IndexVersion is independent
of the file format version.

## Original-state dump semantics

The v1 encoding supports unmodified dumps through `Metadata.IndexVersion=0`
and migrations through `Metadata.IndexVersion=1`. Both use the current record
encoding and validation rules. Other index versions are invalid.

For a dump, ToolVersion must be nonempty and Genesis must be nonzero.
IndexHeadNumber, IndexHeadHash, IndexDigest, AllowanceCount and MemberCount must
all be zero, and Inputs must be empty. Source identity comes from the preserved
header and Genesis, without implying any event-index evidence.

SourceRoot, ConvertedStateRoot and FinalStateRoot must all equal the source
header root. ConvertedCounts must equal Counts. All five conversion amount
strings must be exactly `"0"`; holder counts, AuditHeight and AuditHash must be
zero. This means no conversion was performed, not that source token balances
were zero. Reader and Writer enforce these header/footer constraints in addition
to normal record, code, inventory, root and checksum validation. A modified state
cannot be labeled a dump even if it has a valid checksum.

## Bounded block transport

After the preamble, each block is `[rawLength:uint32, encodedLength:uint32, data]`.
Raw blocks are exactly 256 KiB except the last, which may be shorter. Neither
length may be zero, and encodedLength is at most 512 KiB. A final pair of zero
lengths terminates the physical file; anything after it is invalid.

For none, the lengths must match. Zstd blocks use one independent canonical frame:
klauspost/compress v1.20.1 `SpeedDefault`, concurrency 1, checksum enabled. The
reader bounds decompression and checks canonical encoding, rejecting concatenated
or alternative frames. A codec change requires explicit review and a corresponding corpus update
if its bytes change; the format version stays at v1. No skippable/extra frames are accepted.

## Logical records

Concatenated decoded blocks carry `[kind:uint8, payloadLength:uLEB128, payload]`.
The length is a nonzero uint32 encoded with canonical unsigned LEB128 (least
significant seven-bit group first, high bit indicating continuation). It uses at
most five bytes; overflows, redundant terminal zero groups and per-kind limit
violations are rejected before reading payloads. Record headers and payloads may
cross block boundaries. The digest uses the exact encoded header bytes.

A compact integer is `[byteLength:uint8, minimalBigEndianValue]`. Zero has length
0 and no value bytes. Nonzero values cannot start with zero. A uint256 permits
0..32 value bytes and a uint64 permits 0..8. No payload may contain trailing bytes.

| Kind | Payload |
| --- | --- |
| 1 AccountStart | address20, flags1, compact balance (uint256), compact nonce (uint64), optional codeHash32; maximum 95 bytes |
| 2 StorageEntry | StorageKeyHash32, compact nonzero value (uint256); maximum 65 bytes |
| 3 AccountEnd | compact slotCount (uint64), storageRoot32 only when count is nonzero; maximum 41 bytes |
| 4 CodeEntry | codeHash32, nonempty code (maximum 128 MiB) |
| 5 Footer | RLP Summary (maximum 1 MiB), SHA-256 digest32 |

AccountStart flags bit 0 indicates that a nonempty codeHash follows the nonce;
all other bits must be zero. When absent, the hash is restored to Ethereum's
`EmptyCodeHash` (keccak256 of empty bytes), not an all-zero hash. Explicit empty
or all-zero code hashes are rejected. Empty code does not imply empty storage.

AccountEnd count 0 implies `EmptyRootHash` with no root bytes. The Writer still
checks the caller's root and count against the streamed storage before accepting
the record. Nonzero counts retain the explicit root and both are verified.
Storage values decode back into zero-left-padded 32-byte words; their already
hashed keys are never rehashed. Public Go records retain their original types.

A zero-balance, zero-nonce, empty-code account without storage occupies 28 logical
bytes including both record headers (formerly 142). This excludes preamble,
footer and block overhead; zstd savings depend on the data distribution.

Each account has exactly one start/end pair, with storage records between them.
Accounts strictly increase by keccak(address); slots strictly increase by their
already-hashed key. Code records follow all accounts; hash order is unrestricted.
The exporter uses first source-account reference order, then a new conversion
runtime, then new alloc codes in ascending keccak(address) order. Reused hashes
keep their original position; codes without final references are omitted. The
Reader validates uniqueness and completeness, not this source-dependent order.
There is exactly one code record per referenced nonempty code hash. Missing,
duplicate or orphan code and zero storage words are invalid.

The SHA-256 commits to the complete preamble, then the exact headers/payloads of
records 1–4, then the Footer Summary RLP. It excludes the Footer record header and
the digest itself. Block framing is validated separately and is canonical.

Summary RLP fields are SourceRoot, ConvertedStateRoot, FinalStateRoot, Counts,
ConvertedCounts, Conversion. Counts is `[Accounts, StorageSlots, Codes, CodeBytes]`.
Conversion contains canonical uint256 decimal strings for OriginalSupply,
BalanceSum, MigratedNative, RemainingSupply, SelfBalance, followed by EOA/converted/
retained holder counts, AuditHeight and AuditHash. The original header root must
equal SourceRoot; the reconstructed output must equal FinalStateRoot.

The verifier rebuilds every storage trie from minimal nonzero RLP words, then
account tries from canonical Ethereum `[nonce,balance,storageRoot,codeHash]` RLP.
It checks all code hashes, references, counts, footer checksum and physical EOF.
For migration, ConvertedStateRoot/counts are provenance, not independently
reconstructible from final state after an alloc overlay. For dump, they must
equal the independently verified final root/counts.

## Frozen corpus

The 2026-10-17 code-order change retains the v1 version byte.
`statefile/testdata/v1-none.bin` and `v1-zstd.bin` are the current frozen samples;
tests read them and compare current Writer bytes. Their single-code payload
is unchanged, so these fixtures need no regeneration. Multi-code ordering and
nonadjacent duplicate tests separately cover the new rule. The corpus checks
the current encoding; it does not establish compatibility with earlier revisions.
The checkpoint schema simplification does not change statefile encoding or these
binary fixtures. The current event JSON fixture is
`internal/ovmevents/testdata/v1.json`; its test pins exact encoder bytes and the
independently calculated normalized index digest for the address-set schema.
The member values in that digest are empty; this is an intentional v1 digest
change. The fixture must retain the exact streaming encoder bytes, including
whitespace, rather than being reformatted as pretty JSON.
Intentional candidate generation uses
`L2STATE_FORMAT_CANDIDATE=/new/directory go test ./statefile -run TestFormatCandidate`.
The destination must not exist. Review the format decision and byte diff before
adding a corpus; ordinary tests never regenerate it.
