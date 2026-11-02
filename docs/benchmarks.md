# Benchmarks

Measurements from real hosted mailboxes, run from a Mac over one home
connection. They show what one setup did on one day, not what a hoster
guarantees.

## 2026-11-02: Infomaniak to cyon

Built from `69c6aaf` without changes, with Go 1.27.3 on darwin/arm64.

### Test data

Synthetic mail, no real content: 17,180 messages, 1.53 GB, 10 folders.

| Kind | Messages | Size |
|---|---|---|
| Small | 16,158 | 5–100 KB |
| With attachments | 1,013 | 100 KB – 8 MB |
| Large | 9 | 20–25 MB |

The set includes messages without a Message-ID, duplicates, a Message-ID
conflict, and missing or invalid dates. Attachments are random bytes, so they do
not compress.

### Servers

| | Infomaniak | cyon |
|---|---|---|
| Connections held | 16 of 16 probed | 16 of 16 probed |
| MULTIAPPEND | yes | yes |
| UIDPLUS, CONDSTORE | yes | yes |
| COMPRESS=DEFLATE | no | yes |
| Personal namespace | none, delimiter `/` | `INBOX.`, delimiter `.` |

### Runs

| Run | Direction | Data | Connections | Time | Rate |
|---|---|---|---|---|---|
| Seed | local to Infomaniak | 17,180 msgs, 1.53 GB | 16 dest | 1m18s | 221 msg/s, 19.7 MB/s |
| Baseline, stopped | Infomaniak to cyon | 793 of 17,080 msgs | 16 / 16 | 9m55s | 1.3 msg/s |
| Read only | Infomaniak to local | 200 msgs, 17 MB | 16 source | 1.3s | 149 msg/s, 12.9 MB/s |
| Write only | local to cyon | 200 msgs, 17 MB | 16 dest | 2m19s | 1.4 msg/s, 0.12 MB/s |
| Write only, stopped | local to cyon | 127 of 200 msgs | 1 dest | 2m0s | 1.1 msg/s |

### Findings

- APPEND to cyon limited every run: reading was about 100 times faster.
- Going from 1 to 16 connections raised the rate only from 1.1 to 1.4 msg/s,
  so more connections will not fix this. The baseline wrote to three folders at
  once and stayed at 1.3 msg/s, which points to a limit per account rather than
  per folder.
- The same data was accepted at 221 msg/s by Infomaniak, so this is not a client
  limit.
- At this rate the full set would take about 3.5 hours.

### MULTIAPPEND

Measured local to cyon with a standalone client over one connection, because
go-imap v2 has no MULTIAPPEND command. Every run was confirmed with `STATUS`.

| Messages per APPEND | Messages | Size | Time | Rate | Per command |
|---|---|---|---|---|---|
| 1 | 40 | 5 MB | 32.1s | 1.2 msg/s, 0.17 MB/s | 804 ms |
| 10 | 200 | 17 MB | 20.0s | 10.0 msg/s, 0.87 MB/s | 1.0 s |
| 50 | 200 | 17 MB | 3.5s | 56.9 msg/s, 4.93 MB/s | 878 ms |
| 200 | 1,000 | 96 MB | 12.6s | 79.3 msg/s, 7.65 MB/s | 2.5 s |
| 1, messages over 1 MB | 15 | 79 MB | 15.1s | 1.0 msg/s, 5.21 MB/s | 1.0 s |

- cyon's limit counts commands, not messages or bytes: one APPEND takes about
  1 s whether it holds one small message, one large one or 50 small ones.
- Batches of 200 are about 66 times faster than single APPENDs. At 7.65 MB/s,
  20 GB would take about 44 minutes.

### With MULTIAPPEND in the sync

The stopped baseline, resumed from its state database with batches of up to 200
messages or 32 MiB, at 16 / 16 connections.

| Run | Direction | Data | Time | Rate |
|---|---|---|---|---|
| Baseline, resumed | Infomaniak to cyon | 16,287 msgs, about 1.45 GB | 5m4s | 53.6 msg/s, about 4.8 MB/s |

- That is 41 times the rate of single APPENDs. The whole set would take about
  5.5 minutes instead of 3.5 hours.
- 0 failures and 0 warnings. A repeat run found nothing to copy.
- Reading everything back from cyon confirmed all 17,180 originals through the
  recorded UIDs: bytes identical, flags and internal dates preserved. 150
  messages carry the added `X-Imapsync-Go-Id` stamp. 100 duplicates share a copy
  as intended.
- Each message also gained a `$HasAttachment` or `$HasNoAttachment` keyword from
  a server. It is correct on every message and not set by this tool.
- At 4.8 MB/s, 20 GB would take about 70 minutes. The single-connection
  measurement reached 7.65 MB/s, so some of the gap is in the pipeline.

### Folder mapping in these runs

These runs predate namespace detection, so each new folder on cyon was mapped
into the `INBOX.` namespace with `--map`. Detection now plans the same names on
its own: a dry run without `--map` against the finished migration found all
17,180 messages already done.
