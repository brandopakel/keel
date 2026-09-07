# AOF staging and opaque replication allocation

Committed AOF staging used to truncate its outer slices while retaining their
backing arrays' references to command parts. A SET with an 8 MiB value, 64 KiB
key and expiry left references accounting for 8,519,801 bytes after encoding.
Staging now clears those references when consumed. Closing a drained AOF also
releases its idle buffer. The focused test verifies removal, the surviving value
and two replays; all three repetitions retain zero staged payload bytes.

Protocol-2 opaque updates previously constructed a full state replacement before
checking the 64 MiB delta limit. A just-oversized CMS update allocated 201,357,472
bytes before invalidating the history. The candidate sizes the aggregate of all
dirty-key replacements and expiry records before constructing any image. The
same refusal allocates 728 bytes locally and preserves the acknowledged mutation
while requiring snapshot recovery. Two individually admissible 36 MiB images
are also refused before construction when their aggregate exceeds the limit.

Accepted replacements append their KEL1 payload directly into one preallocated
RESP body, preserving checksums and exact state without intermediate binary and
string copies. Tests replay every stored type, deletion and absolute expiry,
and exercise replica snapshot, delta and checkpoint recovery. This allocation
check occurs after the primary's logical command: refusal selects replication
snapshot recovery rather than returning an error for an already executed write.

These fixes do not establish a process-wide transient reservation system. Client
replies, retained request buffers, AOF capacity, worker-owned bytes, replication
history and rewrite images still need aggregate admission before construction.
An accepted opaque image still serializes synchronously, and snapshot fallback
does not make large-key catch-up efficient. The frozen long soaks predate this
candidate; local measurements are diagnostics rather than dedicated-host results.
