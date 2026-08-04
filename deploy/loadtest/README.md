# In-region load testing (Fly + vegeta/loadshape over 6PN)

This runs the `benchmark/` scripts (see
[`../../benchmark/README.md`](../../benchmark/README.md)) from a Fly
machine in the **same org + region** as yasdb, hitting the target over
**6PN private networking** (`http://yasdb.internal:4437`). This bypasses
the public Anycast edge, TLS, and the proxy/concurrency limits, so you
measure the database, not the internet. (Testing
`https://yasdb.fly.dev` from your laptop measures WAN latency; in-region
it drops to sub-millisecond.)

## Why a separate app

The load generator must not share CPU with the server; that was the
localhost problem. A second Fly app in the same region gives
`vegeta`/`loadshape` their own dedicated cores, plus a private,
low-latency path to the target.

## One-time setup

Run this from the **repo root** (the build context must include
`benchmark/`):

```sh
fly apps create yasdb-loadtest
fly deploy -c deploy/loadtest/fly.toml
```

The machine just `sleep`s; you exec into it to run benchmarks. Confirm
connectivity:

```sh
fly ssh console -a yasdb-loadtest -C "curl -s http://yasdb.internal:4437/__health"
# -> ok
```

> The target must be in the **same Fly org**, so `.internal` can
> resolve, and in the same **region** (`syd` here), for lowest latency.
> `.internal` points straight at the machine's process port (4437), not
> the proxy — exactly what you want.

## Run tests

```sh
# single test (helper wraps `fly ssh console`)
./deploy/loadtest/run.sh max-throughput WORKERS=400 STREAMS=32
./deploy/loadtest/run.sh ramp-append PEAK_RATE=20000 RAMP=5m
./deploy/loadtest/run.sh smoke
```

Or interactively:

```sh
fly ssh console -a yasdb-loadtest
# inside the machine (cwd is /benchmark):
BASE_URL=http://yasdb.internal:4437 WORKERS=400 STREAMS=32 ./max-throughput.sh
loadshape -target-url http://yasdb.internal:4437 \
  -admin-url http://yasdb.internal:9091/__admin/bulk-provision \
  -streams 32 -op append -stages '[{"duration":"5m","target":20000}]'
```

## Key difference in-region: use fewer workers

Throughput equals concurrency divided by latency. Over the WAN (about
130 ms), you needed about 1200 concurrent workers to push a few
thousand rps. In-region latency is sub-millisecond, so a **few hundred
workers saturate the server**. Start at `WORKERS=200–400` and increase
only if throughput keeps rising. Blasting 1200 workers in-region just
deepens queues without adding throughput.

## Find the real ceiling

Watch **both** sides during a run. Whichever pegs first is the wall:

```sh
fly ssh console -a yasdb        -C top   # the server
fly ssh console -a yasdb-loadtest -C top   # the load generator (vegeta/loadshape)
```

- `yasdb` pegged → server-bound: scale vCPUs (`performance-2x`→`4x`), or
  tune `-flush` / durability.
- `yasdb-loadtest` pegged → the generator is the limit: bump `cpus` in
  this app's `fly.toml`, or add a second load-generator machine/region.
- Neither pegged → still worker/latency-limited: raise `WORKERS`.

## Teardown (stop billing)

```sh
fly apps destroy yasdb-loadtest        # removes the app + machine
# or keep it and just stop the machine between runs:
fly machine list -a yasdb-loadtest
fly machine stop <id> -a yasdb-loadtest
```
