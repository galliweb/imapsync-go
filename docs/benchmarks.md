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
  per folder. The cause is not known yet.
- Whether the limit counts messages or bytes is not measured yet. The same data
  was accepted at 221 msg/s by Infomaniak, so this is not a client limit.
- At this rate the full set would take about 3.5 hours.

### Known issue

The NAMESPACE prefix never reaches the folder planner: `folder.Options.DestPrefix`
is not set anywhere. New folders on cyon would be planned outside the `INBOX.`
namespace, so these runs mapped each one with `--map`.
