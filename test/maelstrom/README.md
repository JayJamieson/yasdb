# Maelstrom correctness check

An independent, black-box log-consistency check of yasdb's append and read path
using [Jepsen Maelstrom](https://github.com/jepsen-io/maelstrom)'s `kafka`
workload.

[`../../cmd/maelstrom-adapter`](../../cmd/maelstrom-adapter) is a pure-Go
bridge. Maelstrom speaks newline-delimited JSON to it, and it translates each
kafka RPC into a yasdb HTTP call.

| kafka RPC                                     | yasdb                                                            |
| --------------------------------------------- | ---------------------------------------------------------------- |
| `send {key, msg}`                             | `POST /mlst/<key>` with one JSON message. The offset is the seq. |
| `poll {offsets}`                              | `GET /mlst/<key>?offset=<seq>`, which returns `[[offset, msg], …]` |
| `commit_offsets` and `list_committed_offsets` | the in-process committed-offset store                            |

A Kafka offset is the message's yasdb sequence number. Run with one Maelstrom
node so committed offsets share a process. Concurrency comes from
`--concurrency` and `--rate`, all hitting one shared yasdb server. Maelstrom's
checker then verifies that the observed history is a valid log. Offsets are
unique and monotonic per key, and no message is lost, duplicated, or reordered.

## Run

Both scripts take the durability mode as a required first argument. yasdb treats
any `-durability` value other than `notifier` as sync, so a missing or misspelled
mode would run sync, print a pass, and leave you believing the pass came from
notifier. The scripts reject the mode rather than default it.

```sh
# A JDK is required. Download Maelstrom and point MAELSTROM at its launcher.
MAELSTROM=/path/to/maelstrom/maelstrom ./run.sh sync
MAELSTROM=/path/to/maelstrom/maelstrom ./run.sh notifier   # async durability

# settings: CONCURRENCY (default 10), RATE (200), TIME (20s)
CONCURRENCY=20 RATE=500 TIME=30 MAELSTROM=… ./run.sh notifier

# READ=long-poll polls via ?offset=&live=long-poll, so the checker covers the
# live-read and in-memory record-cache path as well as catch-up reads. The
# server runs with a short -longpoll-timeout (LPTIMEOUT, default 500ms) so a
# caught-up poll returns 204 promptly.
READ=long-poll MAELSTROM=… ./run.sh notifier
```

A pass ends with `Everything looks good! ヽ(‘ー`)ノ` and `:valid? true`.

## Result

Both durability modes pass, measured at concurrency 10, 200 ops/s, over 20 s.

- `sync`. 1615 sends and 1643 polls, `:valid? true`, availability 99.5%.
- `notifier`. `:valid? true`, availability 99.6%, zero indeterminate send and
  poll ops.
- `READ=long-poll` in both modes. `:valid? true`, availability about 99%. The
  same checker covers the live-read and record-cache path.

This is the correctness half of the `-durability notifier` evaluation. Under
`notifier` an ack comes from the durable-seq watcher instead of a blocking
write, and log consistency holds exactly as it does under `sync`. Throughput
numbers for the same mode are in [`../../BENCHMARKS.md`](../../BENCHMARKS.md).

## Crash and durability fault injection

[`run-nemesis.sh`](./run-nemesis.sh) turns the same checker into a crash
recovery and durability test. Maelstrom v0.2.3's CLI only wires up `--nemesis
partition`, which does nothing to a single-node server, so the harness injects
the fault itself. In this mode the adapter supervises yasdb as a child process
on a persistent file store, and a chaos loop (`YASDB_CHAOS_MS`) kills yasdb with
`SIGKILL` and restarts it on a cadence while the workload runs. The child dies
with the adapter via `PDEATHSIG`, and each restart forces a real SlateDB
recovery, replaying the WAL from the store. The kafka checker then verifies that
every acked write survived the hard kills. That is yasdb's "ack only after
durable" contract, tested across crashes.

```sh
# crash yasdb every 5s during a 60s kafka run:
MAELSTROM=/path/to/maelstrom/maelstrom ./run-nemesis.sh sync
MAELSTROM=/path/to/maelstrom/maelstrom ./run-nemesis.sh notifier

# settings: CHAOS_MS (crash interval, 5000), QUIESCE (quiet seconds before the
# time limit, 12), CONCURRENCY (8), RATE (100), TIME (60s)
CHAOS_MS=2500 RATE=200 TIME=45 MAELSTROM=… ./run-nemesis.sh notifier
```

The adapter writes crash and recovery events to Maelstrom's per-node log,
`store/<test>/node-logs/n0.log`, as the lines `chaos: SIGKILL` and `chaos: yasdb
recovered in <duration>`. `--node-count 1` is required, because there is one
writer and one store.

### Chaos stops before the run ends

Maelstrom runs a final read phase after the time limit. It assigns every key to
one client and polls each stream until it is empty, and the checker decides
`:unseen` there. A crash that lands in that phase leaves the newest acked
records unpolled, and the checker reports them as `:unseen`.

So the chaos loop stops `QUIESCE` seconds before the time limit and yasdb stays
up from then on (`YASDB_CHAOS_WINDOW_MS`, which `run-nemesis.sh` computes as
`TIME - QUIESCE`). The run still crashes yasdb throughout the workload, so it
still exercises crash recovery under load. A record genuinely lost by the *last*
crash is still caught, because the final reads now reach the tail of every
stream. The log line `chaos: window closed` marks the end of the crash window.

The 12 s default is about fifteen times the worst crash-plus-recovery cycle this
harness measures, 404 ms to 810 ms over 189 cycles at `CHAOS_MS=3000`,
`RATE=100`, and `CONCURRENCY=8`. A crash issued just before the deadline still
recovers long before the final reads start. The margin is wide on purpose. An
undersized window brings back the false `:unseen`, and a larger one costs only a
few of the run's crashes. To re-measure the cycle on your own machine, run
`grep 'recovered in' store/current/node-logs/n0.log`. Leave
`YASDB_CHAOS_WINDOW_MS` unset to crash for the whole run. That is the old
behaviour.

### `:unseen` means no reader saw it, not that yasdb lost it

`:unseen` under `:bad-error-types` means no poll observed those messages. It does
not mean yasdb dropped them. The checker cannot tell a record that was never
written from one that was written and never read, so a reader that ran out of
time looks exactly like a lost write.

To tell the two apart, keep the store and read the reported keys back out of it:

```sh
# 1. note the keys and values the checker printed under :messages, such as
#    :messages {"148" (13), "158" (4)}
# 2. re-run with the store outside the script's temp dir, so it survives:
YASDB_DATA=/tmp/yasdb-unseen MAELSTROM=… ./run-nemesis.sh notifier
# 3. serve that store and read each reported key from offset 0:
make run ARGS="-addr 127.0.0.1:4800 -data /tmp/yasdb-unseen"
curl -s 'http://127.0.0.1:4800/mlst/148?offset=0000000000000000_0000000000000000'
```

If the value is in the stream, nothing was lost and the acknowledgement contract
held. Expect it to be the *last* record in that stream. A reader that stopped
early leaves exactly that record unseen. If the value is absent, that is a real
durability bug.

### Result

Both modes stay `:valid? true` under repeated hard kills, with no lost or
duplicated acked write and no yasdb panic or store corruption.

| mode       | crashes | rate    | verdict        | availability |
| ---------- | ------- | ------- | -------------- | ------------ |
| `sync`     | 9 / 45s | 100/s   | `:valid? true` | 95.3%        |
| `notifier` | 9 / 45s | 100/s   | `:valid? true` | 95.7%        |
| `notifier` | 15 / 45s | 200/s  | `:valid? true` | 91.0%        |

These crash counts predate the quiesce window, so the same invocations now crash
yasdb fewer times over a shorter window. The verdicts are unchanged.

The availability dips are the recovery windows, where ops go indeterminate while
yasdb replays its WAL. The consistency of the acked log never breaks. This is
the durability half of the `notifier` evaluation. The durable-seq watcher
releases an async ack only once the durable watermark passes the write, so a
hard kill cannot lose an acked record. These runs measure that rather than
assume it.
