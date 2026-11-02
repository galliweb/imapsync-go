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

### Tuning the batches

Writing the three `Archiv` folders, 6,585 messages and 547 MB, local to cyon:

| Messages per batch | Bytes per batch | Connections | Rate |
|---|---|---|---|
| 200 | 32 MiB | 16 | 9.72 MB/s |
| 200 | 32 MiB | 8 | 8.89 MB/s |
| 200 | 32 MiB | 4 | 10.76 MB/s |
| 200 | 32 MiB | 2 | 5.00 MB/s |
| 500 | 64 MiB | 4 | 17.10 MB/s |
| 1,000 | 128 MiB | 4, with a 512 MiB memory limit | 19.99 MB/s |

cyon handled about one command per second however many were waiting, so past
four connections extra commands only queue while holding memory. Bigger batches
are what helps. The sync now sends up to 500 messages or 64 MiB per batch, with
four batches in flight across all folders.

### Full runs after tuning

The whole set, Infomaniak to cyon, into a fresh folder each time. 17,080 messages,
about 1.52 GB, 16 / 16 connections, 0 failures, 0 warnings.

| Batches | Time | Rate |
|---|---|---|
| Sealed before a slot was free | 3m27s | 82.5 msg/s, about 7.4 MB/s |
| Sealed by taking a slot | 1m51s | 153.6 msg/s, about 13.7 MB/s |

- With nine folders running, each saw a ninth of the source, so a batch sealed
  early froze small. A batch that keeps growing until a slot is free stays full.
- Reading everything back confirmed all 17,180 originals through the recorded
  UIDs again: bytes, flags and internal dates.
- At 13.7 MB/s, 20 GB would take about 24 minutes.

### The other direction

cyon to Infomaniak into a fresh folder: 17,080 messages, about 1.52 GB, in 45s,
at 379 msg/s and about 34 MB/s, with 0 failures and 0 warnings. The `INBOX.`
prefix was dropped from every folder name without `--map`. Following all 17,180
originals through Infomaniak, cyon and back to Infomaniak by their recorded UIDs
found bytes, flags and internal dates intact.

### Metanet

Metanet held 16 of 16 probed connections and offers MULTIAPPEND and
COMPRESS=DEFLATE. Its personal namespace is `INBOX.`, delimiter `.`. Runs on the
same set, at 16 / 16 connections, each into a fresh folder:

| Run | Messages | Time | Rate |
|---|---|---|---|
| Local to Metanet | 17,180 | 40s | 428 msg/s, about 38 MB/s |
| Metanet to cyon | 17,080 | 1m44s | 164.5 msg/s, about 14.6 MB/s |
| Metanet to Infomaniak | 17,080 | 21s | 830 msg/s, about 74 MB/s |

- 0 failures and 0 warnings in all three. The upload created every new folder
  under `INBOX.` without `--map`.
- Reading both destinations back confirmed all 17,180 originals through the
  recorded UIDs: bytes, flags and internal dates.
- Metanet to cyon matches Infomaniak to cyon, which points at cyon as the limit
  on that route.

### 17.7 GB

A larger synthetic set of 202,045 messages and 17.74 GB, with the same mix, at
16 / 16 connections and default memory limit:

| Run | Messages | Time | Rate |
|---|---|---|---|
| Local to Infomaniak | 202,045 | 1m52s | 1,805 msg/s, about 158 MB/s |
| Infomaniak to cyon | 200,878 | 23m6s | 144.9 msg/s, about 12.7 MB/s |

- 0 failures and 0 warnings. 1,167 duplicates were left out as intended.
- The rate held between 142 and 205 msg/s for the whole run, with no sign of the
  server throttling sustained load.
- Reading everything back confirmed all 202,045 originals through the recorded
  UIDs: bytes, flags and internal dates. A repeat run found nothing to copy.
- At this rate 20 GB takes about 26 minutes, inside the 20 to 45 minute target.

### A real mailbox

A business mailbox from Metanet to cyon: 12 folders, 4,916 messages, 1.74 GB,
averaging about 350 KB, the largest 41 MB. It took 2m19s at 35 msg/s and about
12.5 MB/s, with 0 failures and 0 warnings. Every folder's count matched the
source, a repeat run found nothing to copy, and no `--map` was needed. At that
rate 20 GB would take about 27 minutes.

Larger messages lower the message rate but not the byte rate, which stayed close
to the synthetic runs. Contents, flags and dates were not compared on this run.

A webmail holds its folder list for the session, so folders created or deleted
during a migration may not show until the next login.

### Two migrations at once

Two runs started together from one Mac, each its own process with its own state
database, at 8 / 6 connections. Both read the 1.53 GB set from the same Infomaniak
account and wrote to a fresh folder. Each route then ran alone at the same
settings, minutes later.

| Route | Together | Alone |
|---|---|---|
| Infomaniak to cyon | 1m17s, 222 msg/s, about 20 MB/s | 1m25s, 201 msg/s, about 18 MB/s |
| Infomaniak to Metanet | 46s, 371 msg/s, about 33 MB/s | 47s, 363 msg/s, about 32 MB/s |

- 0 failures in all four runs, and neither route slowed down next to the other.
- The two runs overlapped for the first 46s. They shared a source account but not
  a destination, so this says nothing about two migrations into one hoster.
- Reading both parallel copies back confirmed all 17,180 originals through the
  recorded UIDs: bytes, flags and internal dates. Repeat runs found nothing to
  copy. The runs alone were only timed.
- Peak memory was 833 MB to cyon and 510 MB to Metanet together, and 904 MB to
  cyon alone, at the default `--memory-limit` of 256 MiB. That flag covers message
  data, not the whole process.
- cyon took 18 to 20 MB/s here against 13.7 MB/s in the earlier full runs at
  16 / 16. Both the connection count and the time of day differ, so this does not
  show which one helped.

### Other measurements

| Run | Data | Time | Rate |
|---|---|---|---|
| Infomaniak to local files | 17,180 msgs, 1.53 GB | 58s | 299 msg/s, 26.6 MB/s |
| cyon to local files | 17,080 msgs, about 1.52 GB | 57s | 301 msg/s, 26.8 MB/s |
| Local to Infomaniak, batched | 6,585 msgs, 547 MB | 1.7s | 3,840 msg/s, about 320 MB/s |
| Infomaniak to Infomaniak, batched | 6,585 msgs, 547 MB | 9.2s | 714 msg/s, about 59 MB/s |
| cyon to Infomaniak, batched | 6,585 msgs, 547 MB | 8.6s | 767 msg/s, about 64 MB/s |

- The local write was checked by reading back all 6,621 originals. Without
  batches the same server took 19.7 MB/s, so that figure was never the uplink.
- The two runs to local files stop at the same rate because of the local store,
  which writes one file per message. They say nothing about either server.
- With a fast destination, both servers read at over 700 msg/s with one FETCH per
  message. That is several times what cyon can store, so reading does not limit
  a migration to cyon and batching FETCH would not speed one up.

### Folder mapping in these runs

These runs predate namespace detection, so each new folder on cyon was mapped
into the `INBOX.` namespace with `--map`. Detection now plans the same names on
its own: a dry run without `--map` against the finished migration found all
17,180 messages already done.
