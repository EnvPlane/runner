# Root metadata receipt and unsupported extended attributes

Status: root header/restoration already implemented; acceptance coverage and
explicit RootReceipt added in this iteration. Linux attribute refusal added.

Problem: a bytes-only target or a skipped volume root can leave app UID/GID
unable to write a newly provisioned root:root 0755 volume. The existing `.` root
header must remain mandatory, bounded, hashed and restored last. Unsupported
ACL/xattrs must not silently disappear under a full-filesystem preservation claim.

Codex implementation prompt: In runner/internal/pvccopy only, bind numeric root
UID/GID/mode explicitly into Receipt and test root 0770 with different ownership
versus target 0755, including non-root macOS compatible group changes. On Linux,
query visible xattr name size using the opened file descriptor, refuse any
attribute or enumeration error, and check root/directories/files before source
export and target completion. Do not allocate arbitrary attribute lists, grant
CAP_SYS_ADMIN or modify source metadata. Add Linux ACL/xattr tests and root app
write-access tests. State unsupported hidden attributes and non-Linux detection
limits explicitly. No runtime wiring, source writes, policy grants or push.
