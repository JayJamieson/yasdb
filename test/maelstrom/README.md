# Maelstrom correctness check

An independent, black-box log-consistency check of yasdb's append/read path using
[Jepsen Maelstrom](https://github.com/jepsen-io/maelstrom)'s **kafka** workload.

[`../../cmd/maelstrom-adapter`](../../cmd/maelstrom-adapter) is a pure-Go bridge:
Maelstrom speaks newline-delimited JSON to it, and it translates each kafka RPC
to yasdb HTTP —

| kafka RPC                | yasdb                                                      |
| ------------------------ | ---------------------------------------------------------- |
| `send {key, msg}`        | `POST /mlst/<key>` (one JSON message) -> offset = seq       |
| `poll {offsets}`         | `GET /mlst/<key>?offset=<seq>` -> `[[offset, msg], …]`      |
| `commit_offsets` / `list_committed_offsets` | in-process committed-offset store          |

A Kafka offset is the message's yasdb sequence number. Run with one Maelstrom
node so committed offsets share a process; concurrency comes from `--concurrency`
/ `--rate`, all hitting one shared yasdb server. Maelstrom's checker then
verifies the observed history is a valid log: offsets unique and monotonic per
key, and no message lost, duplicated, or reordered.

## Run

```sh
# JDK required; download Maelstrom and point MAELSTROM at its launcher:
MAELSTROM=/path/to/maelstrom/maelstrom ./run.sh sync
MAELSTROM=/path/to/maelstrom/maelstrom ./run.sh notifier   # async-durability knob

# knobs: CONCURRENCY (default 10), RATE (200), TIME (20s)
CONCURRENCY=20 RATE=500 TIME=30 MAELSTROM=… ./run.sh notifier

# READ=long-poll polls via ?offset=&live=long-poll, so the checker also covers
# the live-read + in-memory record-cache path (not just catch-up reads). The
# server runs with a short -longpoll-timeout (LPTIMEOUT, default 500ms) so a
# caught-up poll returns 204 promptly.
READ=long-poll MAELSTROM=… ./run.sh notifier
```

A pass ends with `Everything looks good! ヽ(‘ー`)ノ` and `:valid? true`.

## Result

Both durability modes pass (validated at concurrency 10, 200 ops/s, 20 s):

- **`sync`** — 1615 sends / 1643 polls, `:valid? true`, availability 99.5%.
- **`notifier`** — `:valid? true`, availability 99.6%, zero indeterminate
  send/poll ops.
- **`READ=long-poll`** (both modes) — `:valid? true`, availability ~99%,
  validating the live-read + record-cache path under the same checker.

This is the correctness half of the `-durability notifier` evaluation: the
async-durability knob (ack via the durable-seq watcher instead of blocking each
write) preserves log consistency, matching `sync`. Throughput numbers for the
same knob are in [`../../BENCHMARKS.md`](../../BENCHMARKS.md).

## Fault injection (crash / durability)

[`run-nemesis.sh`](./run-nemesis.sh) turns the same checker into a **crash
recovery + durability** test. Maelstrom v0.2.3's CLI only wires up `--nemesis
partition` (a no-op for a single-node server), so we inject the fault ourselves:
in this mode the adapter **supervises yasdb as a child** on a persistent file
store, and a chaos loop (`YASDB_CHAOS_MS`) `SIGKILL`s and restarts it on a
cadence while the workload runs. The child dies with the adapter via `PDEATHSIG`,
and each restart forces a real SlateDB recovery (WAL replay) from the store. The
kafka checker then verifies that **every acked write survived the hard kills** —
i.e. yasdb's "ack only after durable" contract holds across crashes.

```sh
# crash yasdb every 5s during a 60s kafka run:
MAELSTROM=/path/to/maelstrom/maelstrom ./run-nemesis.sh sync
MAELSTROM=/path/to/maelstrom/maelstrom ./run-nemesis.sh notifier

# knobs: CHAOS_MS (crash interval, 5000), CONCURRENCY (8), RATE (100), TIME (60s)
CHAOS_MS=2500 RATE=200 TIME=45 MAELSTROM=… ./run-nemesis.sh notifier
```

Crash/recovery events are logged to Maelstrom's per-node log
(`store/<test>/node-logs/n0.log`, lines `chaos: SIGKILL` / `chaos: yasdb
recovered`); `--node-count 1` is required (single writer, single store).

### Result

Both modes stay `:valid? true` — zero lost or duplicated acked writes, no yasdb
panic or store corruption — under repeated hard kills:

| mode       | crashes | rate    | verdict        | availability |
| ---------- | ------- | ------- | -------------- | ------------ |
| `sync`     | 9 / 45s | 100/s   | `:valid? true` | 95.3%        |
| `notifier` | 9 / 45s | 100/s   | `:valid? true` | 95.7%        |
| `notifier` | 15 / 45s | 200/s  | `:valid? true` | 91.0%        |

Availability dips are the recovery windows (indeterminate ops while yasdb replays
its WAL); the *consistency* of the acked log is never violated. This is the
durability half of the `notifier` evaluation: async acks via the durable-seq
watcher are only released once the durable watermark passes the write, so a hard
kill can never lose an acked record — confirmed empirically, not just by
construction.
