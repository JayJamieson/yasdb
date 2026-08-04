# Deploying yasdb on Fly.io

This guide deploys yasdb as a plain Fly.io app, using `flyctl`, with
[Tigris](https://www.tigrisdata.com/) (Fly's S3-compatible object
storage) as the SlateDB backend.

---

## 1. Hard constraints

1. **Single writer — never scale above 1 machine.** SlateDB is
   single-writer per database. Two machines writing the same
   store/prefix corrupt it. yasdb v1 is a single-node deployment; run
   exactly one machine. (Scaling reads via read-only replicas is future
   work.)
2. **The native library ships in the image.** yasdb links
   `lib/libslatedb_uniffi.so` via cgo. The `Dockerfile` bundles it and
   bakes an rpath, so the runtime needs no `LD_LIBRARY_PATH`.
3. **Health probe:** yasdb serves `GET /__health` → `200 ok`.
   `fly.toml`'s `[[http_service.checks]]` already points at it.
4. **Memory:** SlateDB pulls in a Tokio runtime and a memtable, so give
   the machine **at least 512 MB**. `fly.toml`'s `[[vm]]` block sets
   this.

---

## 2. Build and push the image

The `Dockerfile` at the repo root is a multi-stage build: a cgo builder
stage, then a `debian-slim` runtime stage that carries the `.so`. It
reads storage and port config from the environment, via
`docker-entrypoint.sh`.

```sh
# from the yasdb repo root (must include lib/libslatedb_uniffi.so)
docker build -t registry.fly.io/<your-app>:yasdb-1 .
docker push  registry.fly.io/<your-app>:yasdb-1
```

Any registry Fly can pull works (`registry.fly.io/...`, GHCR, etc.),
though `fly deploy` normally builds and pushes this for you — see §4.

Entrypoint env contract (`docker-entrypoint.sh`):

| Env | Meaning | Default |
| --- | --- | --- |
| `YASDB_STORE` | object-store URL (`s3://<bucket>`); empty means: use a local volume | *(empty)* |
| `YASDB_DB` | DB path/prefix within the store (object-store mode) | `yasdb` |
| `YASDB_DATA` | data dir (local-volume mode) | `/data/yasdb` |
| `YASDB_FLUSH` | WAL flush interval / durable-append latency floor | `10ms` |
| `PORT` | listen port | `4437` |
| `AWS_*` | S3/Tigris credentials + endpoint (object-store mode) | — |

---

## 3. Storage: Tigris vs local volume

**Tigris (recommended).** This is durable object storage, decoupled from
the machine, so a machine restart or move keeps the data. yasdb resolves
`s3://<bucket>` and reads credentials and the endpoint from the standard
AWS environment variables (object_store 0.14 conventions):

```
YASDB_STORE=s3://<bucket>        # NOTE: bucket only — a path in the URL is rejected;
YASDB_DB=yasdb                   #       the prefix goes here instead
AWS_ENDPOINT=https://fly.storage.tigris.dev
AWS_REGION=auto
AWS_ACCESS_KEY_ID=<tigris key id>
AWS_SECRET_ACCESS_KEY=<tigris secret>
# If your bucket needs it: AWS_VIRTUAL_HOSTED_STYLE_REQUEST=true
```

**Local volume (simpler, single-region).** Leave `YASDB_STORE` empty.
Data lands at `/data/yasdb` on the Fly volume (default mount `/data`, 1
GB). This is fine for dev, but the data stays tied to that
volume/region: it does not survive a machine move to another host
unless the volume follows.

---

## 4. Deploy

A [`fly.toml`](../fly.toml) is included at the repo root:

```sh
fly launch --no-deploy --copy-config     # or: fly apps create yasdb
fly storage create --name <bucket> --app yasdb   # Tigris bucket + AWS_* secrets
# edit fly.toml: set YASDB_STORE=s3://<bucket>
fly deploy
```

`fly.toml` pins one always-on machine (`auto_stop_machines = off`,
`min_machines_running = 1`), a `GET /__health` check, and a 512 MB
guest. Never raise the machine count above 1.

Verify:

```sh
curl -s https://<your-app>.fly.dev/__health                        # -> ok
curl -s -X PUT  https://<your-app>.fly.dev/streams/demo -H 'Content-Type: application/json'
curl -s -X POST https://<your-app>.fly.dev/streams/demo -H 'Content-Type: application/json' -d '{"n":1}'
curl -s        "https://<your-app>.fly.dev/streams/demo?offset=-1"   # -> [{"n":1}]
```

Credentials passed via `fly secrets set` or `fly storage create` never
appear in `fly.toml` or the machine config in the clear.

---

## 5. Provisioning the Tigris bucket

Fly's Tigris integration creates a bucket and hands back S3 credentials
as app secrets:

```sh
fly storage create --name <bucket> --app <your-app>
#  -> AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / BUCKET_NAME / AWS_ENDPOINT_URL_S3
```

You can also create the bucket with any S3 client, against the Tigris
endpoint:

```sh
aws s3 mb s3://<bucket> --endpoint-url https://fly.storage.tigris.dev
```

---

## 6. Upgrades, teardown, and the single-writer caveat

```sh
fly deploy               # ships a new image; replaces the single machine
fly apps destroy <app>   # teardown
```

Because there is exactly one writer, an image upgrade **replaces** the
machine. Expect a brief unavailability window while the new machine
boots and opens the store. Do not raise the machine count to avoid this
window: two machines on the same store is a correctness violation, not a
performance tuning knob. If you need higher write throughput, tune
`YASDB_FLUSH` down and rely on yasdb's group commit (see
[`BENCHMARKS.md`](../BENCHMARKS.md)). If you need HA, that requires the
multi-node work noted in [`DESIGN.md`](../DESIGN.md) (sticky routing plus
a writer lease).

---

## 7. Reading metrics from a load test

If you deploy with `YASDB_METRICS_ADDR` set (see `fly.toml`'s
`[metrics]` block and the root `README.md`'s Metrics section), Fly
scrapes yasdb's native SlateDB Prometheus metrics automatically and
surfaces them in the org's built-in Grafana at fly-metrics.net. For a
one-off look after a manual load test, open Grafana and click through
[`deploy/grafana-slatedb-dashboard.json`](../deploy/grafana-slatedb-dashboard.json).
That dashboard is also just a Prometheus instance behind Fly's API, so
you (or an agent helping you) can query it directly over HTTP instead of
screenshotting a browser every time.

### Creating a read-only API token

```sh
flyctl orgs list                       # find your org slug
flyctl tokens create readonly -o <org-slug> -x 24h
```

`-x` sets the token expiry (the default is 20 years — the Fly CLI itself
recommends a shorter one; `24h` usually covers one deploy, load test,
and analysis session). This token can only *read* the org and its
resources: it cannot deploy, scale, or otherwise mutate anything, so it
is safe to paste into a shell or hand to a tool for read-only querying.
If your `flyctl` version prints anything besides the bare token line
(some versions add a leading blank line or a confirmation message),
strip that before using it as a header value. `flyctl tokens create
readonly --help` documents a `-j/--json` flag if you would rather parse
structured output than plain text.

### Querying Prometheus directly

The endpoint is `https://api.fly.io/prometheus/<org-slug>/`, which
speaks the standard Prometheus HTTP API (`/api/v1/query`,
`/api/v1/query_range`, `/api/v1/series`, `/api/v1/labels`,
`/api/v1/label/<name>/values`, `/api/v1/status/tsdb`; remote-read is not
supported). **The auth header depends on the token type** — this is the
part that is easy to get wrong:

| Token (from) | Header |
| --- | --- |
| `flyctl tokens create readonly` / `... create org` | `Authorization: FlyV1 <token>` |
| `flyctl auth token` (your own full-access login token) | `Authorization: Bearer <token>` |

A read-only token needs `FlyV1`, **not** `Bearer`. Using the wrong
scheme is the most common way this silently 401s.
**`flyctl tokens create` already prints the token with the `FlyV1 `
scheme prefix included** (it prints `FlyV1 fm2_...`, not bare
`fm2_...`). Do not prepend `FlyV1` a second time when you build the
header, or you will send `FlyV1 FlyV1 fm2_...` and get a 401:

```sh
FLY_METRICS_TOKEN="$(flyctl tokens create readonly -o <org-slug> -x 24h)"
# $FLY_METRICS_TOKEN is already "FlyV1 fm2_..." — pass it straight through.

# Instant query — current compaction throughput, labeled by app:
curl -sG "https://api.fly.io/prometheus/<org-slug>/api/v1/query" \
  -H "Authorization: $FLY_METRICS_TOKEN" \
  --data-urlencode 'query=slatedb_compactor_total_throughput_bytes_per_sec{app="<your-app>"}' | jq

# Range query — L0 SST count (the compaction-stall signal from BENCHMARKS.md's
# "Compaction stall under load") over the load test window, one point per 15s:
curl -sG "https://api.fly.io/prometheus/<org-slug>/api/v1/query_range" \
  -H "Authorization: $FLY_METRICS_TOKEN" \
  --data-urlencode 'query=slatedb_db_l0_sst_count{app="<your-app>"}' \
  --data-urlencode 'start=2026-08-02T00:00:00Z' \
  --data-urlencode 'end=2026-08-02T00:30:00Z' \
  --data-urlencode 'step=15s' | jq
```

Metric names come from what is already wired up in
[`deploy/grafana-slatedb-dashboard.json`](../deploy/grafana-slatedb-dashboard.json)
and `internal/ds/metrics.go`. Every panel's PromQL expression is a
ready-made `query=` value. Use this same mechanism to re-run the
compactor knob sweep (`-compactor-max-jobs` /
`-compactor-max-subcompactions` / `-l0-flush-parallelism`,
`YASDB_COMPACTOR_MAX_*` in `docker-entrypoint.sh`) against a real Fly
machine, instead of a local box sharing cores with the load generator.
Pull the before/after numbers back with a couple of `curl` calls,
instead of pasting a load-test transcript by hand.
