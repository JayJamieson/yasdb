# Deploying yasdb with provisor

[provisor](https://github.com/JayJamieson/provisor) is a Fly.io provisioning
platform: it creates the Fly app + flycast IP + a data volume + machine(s),
supervises the in-machine process, and routes ingress. This guide deploys yasdb
as a provisor service, with [Tigris](https://www.tigrisdata.com/) (Fly's
S3-compatible object storage) as the SlateDB backend.

---

## 1. Hard constraints

1. **Single writer — `--scale 1` is mandatory.** SlateDB is single-writer per
   database. Two machines writing the same store/prefix corrupts it. yasdb v1 is
   a single-node deployment; run exactly one machine. (Scaling reads via
   read-only replicas is future work.)
2. **The native library ships in the image.** yasdb links
   `lib/libslatedb_uniffi.so` via cgo; the `Dockerfile` bundles it and bakes an
   rpath, so no `LD_LIBRARY_PATH` is needed at runtime.
3. **Health probe:** yasdb serves `GET /__health` → `200 ok`. Use
   `--health-path /__health`.
4. **Memory:** provisor's default guest is 1 shared CPU / 256 MB. SlateDB pulls
   in a Tokio runtime and a memtable; give it **≥ 512 MB**. The CLI can't set the
   guest — use a registered ServiceSpec (§5) for anything beyond a demo.

---

## 2. Build and push the image

The `Dockerfile` at the repo root is a multi-stage build (cgo builder →
`debian-slim` runtime carrying the `.so`). It reads storage/port config from env
via `docker-entrypoint.sh`.

```sh
# from the yasdb repo root (must include lib/libslatedb_uniffi.so)
docker build -t registry.fly.io/<your-app>:yasdb-1 .
docker push  registry.fly.io/<your-app>:yasdb-1
```

Any registry provisor/Fly can pull works (`registry.fly.io/...`, GHCR, etc.).

Entrypoint env contract (`docker-entrypoint.sh`):

| Env | Meaning | Default |
| --- | --- | --- |
| `YASDB_STORE` | object-store URL (`s3://<bucket>`); empty → local volume | *(empty)* |
| `YASDB_DB` | DB path/prefix within the store (object-store mode) | `yasdb` |
| `YASDB_DATA` | data dir (local-volume mode) | `/data/yasdb` |
| `YASDB_FLUSH` | WAL flush interval / durable-append latency floor | `10ms` |
| `PORT` | listen port | `4437` |
| `AWS_*` | S3/Tigris credentials + endpoint (object-store mode) | — |

It also **bridges provisor secrets**: any `PROVISOR_SECRET_<NAME>` is exported as
`<NAME>` if unset, so provisor-injected `PROVISOR_SECRET_AWS_ACCESS_KEY_ID`
becomes `AWS_ACCESS_KEY_ID` for SlateDB.

---

## 3. Storage: Tigris vs local volume

**Tigris (recommended).** Durable object storage, decoupled from the machine, so
a machine restart/move keeps the data. yasdb resolves `s3://<bucket>` and reads
credentials + endpoint from the standard AWS env vars (object_store 0.14
conventions):

```
YASDB_STORE=s3://<bucket>        # NOTE: bucket only — a path in the URL is rejected;
YASDB_DB=yasdb                   #       the prefix goes here instead
AWS_ENDPOINT=https://fly.storage.tigris.dev
AWS_REGION=auto
AWS_ACCESS_KEY_ID=<tigris key id>
AWS_SECRET_ACCESS_KEY=<tigris secret>
# If your bucket needs it: AWS_VIRTUAL_HOSTED_STYLE_REQUEST=true
```

**Local volume (simpler, single-region).** Leave `YASDB_STORE` empty; data lands
at `/data/yasdb` on the provisor volume (default mount `/data`, 1 GB). Fine for
dev, but the data is tied to that volume/region and does not survive a machine
move to another host unless the volume follows.

---

## 4. Provision — quick path (CLI)

Create the Tigris bucket first (§6), then:

```sh
provisor provision yasdb \
  --image registry.fly.io/<your-app>:yasdb-1 \
  --port 4437 \
  --health-path /__health \
  --scale 1 \
  --region syd \
  --env YASDB_STORE=s3://<bucket> \
  --env YASDB_DB=yasdb \
  --env AWS_ENDPOINT=https://fly.storage.tigris.dev \
  --env AWS_REGION=auto \
  --env AWS_ACCESS_KEY_ID=<id> \
  --env AWS_SECRET_ACCESS_KEY=<secret> \
  --env YASDB_FLUSH=10ms
```

Credentials on the command line land in the control-plane store and machine
config in the clear — acceptable for a throwaway, but for anything real use the
registered-service + secrets path (§5, §6).

Verify:

```sh
FLYCAST=$(provisor get yasdb -o json | jq -r .flycast_ip)   # or the app's address
curl -s http://$FLYCAST:4437/__health                        # -> ok
curl -s -X PUT  http://$FLYCAST:4437/streams/demo -H 'Content-Type: application/json'
curl -s -X POST http://$FLYCAST:4437/streams/demo -H 'Content-Type: application/json' -d '{"n":1}'
curl -s        "http://$FLYCAST:4437/streams/demo?offset=-1"   # -> [{"n":1}]
```

---

## 5. Provision — production (registered ServiceSpec)

The CLI's inline path only sets `Image/Port/HealthPath/Env/Scale/Region`. To set
the **guest size**, **volume size**, or attach an **extension**, register a
`ServiceSpec` in the control-plane config and provision it by name
(`--service yasdb`):

```go
// where you build the control-plane Config (cfg.Specs)
cfg.Specs["yasdb"] = extension.ServiceSpec{
    Name:  "yasdb",
    Image: "registry.fly.io/<your-app>:yasdb-1",
    Port:  4437,
    Scale: 1, // single-writer — never > 1
    Guest: &extension.GuestSpec{CPUs: 1, CPUKind: "shared", MemoryMB: 512},
    Volume: &extension.VolumeSpec{Name: "data", MountPath: "/data", SizeGB: 1},
    HealthCheck: &extension.HealthCheckSpec{Path: "/__health"},
    Env: map[string]string{
        "YASDB_STORE":  "s3://<bucket>",
        "YASDB_DB":     "yasdb",
        "AWS_ENDPOINT": "https://fly.storage.tigris.dev",
        "AWS_REGION":   "auto",
        "YASDB_FLUSH":  "10ms",
    },
    // Optional: compose per-instance store env at provision time (§6b).
    Extensions: &extension.ExtensionSet{ /* onProvision: yasdb-onprovision.risor */ },
}
```

```sh
provisor provision yasdb --service yasdb --region syd
```

Credentials come from the extension's returned `secrets` (recorded control-plane
side, injected as `PROVISOR_SECRET_AWS_*`, bridged to `AWS_*` by the entrypoint)
— they never appear on the command line.

---

## 6. Provisioning the Tigris bucket (outside provisor defaults)

provisor creates the Fly **app / IP / volume / machine**, but **not** a Tigris
bucket — that's an external resource you must provision. Its `fly.*` extension
capability covers machines/IPs only (not storage add-ons), so bucket creation is
either done out-of-band or via the extension's generic `http` capability.

### 6a. Manual (reliable) — `fly storage create`

Fly's Tigris integration creates a bucket and hands back S3 credentials:

```sh
fly storage create --name <bucket> --app <your-app>
#  -> AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / BUCKET_NAME / AWS_ENDPOINT_URL_S3
```

Feed those into the `--env` flags (§4) or the ServiceSpec `Env`/secrets (§5). You
can also create the bucket with any S3 client against the Tigris endpoint:

```sh
aws s3 mb s3://<bucket> --endpoint-url https://fly.storage.tigris.dev
```

### 6b. `onProvision` extension — what it can and cannot do

**It cannot create the bucket.** provisor's *control-plane* extension
capabilities are `http.get` (GET-only, gated by a `NetworkEgress` host
allowlist), `secret.generate` (no `secret.read`), and `fly.*` (machines/IPs
only). None can POST to Fly's GraphQL / Tigris provisioning API or read a Fly
token, so the bucket and its credentials must come from §6a.

**What it can usefully do** is compose each instance's *non-secret* store wiring,
so one shared bucket serves the whole fleet with per-instance DB prefixes. The
hook is committed at
[`deploy/extensions/yasdb-onprovision.risor`](../deploy/extensions/yasdb-onprovision.risor)
(it compiles against provisor's risor v2.1.0):

```risor
let bucket = "CHANGE-ME-yasdb"   // your shared Tigris bucket
let app = event["app"]
let dbPrefix = "yasdb/" + app    // per-instance isolation within the bucket
{
    "env": {
        "YASDB_STORE":  "s3://" + bucket,
        "YASDB_DB":     dbPrefix,
        "AWS_ENDPOINT": "https://fly.storage.tigris.dev",
        "AWS_REGION":   "auto",
    },
}
```

Wire it via the ServiceSpec's `Extensions` (§5) so onProvision returns this
`env`, and supply the shared Tigris key/secret as the service's `secrets`
(injected as `PROVISOR_SECRET_AWS_*`, bridged to `AWS_*` by the entrypoint). To
fully automate *bucket creation* inside provisor you'd extend provisor itself —
add an `http.post` (or a Fly add-on) control-plane capability — which is a
provisor change, not a yasdb one.

---

## 6c. Alternative: plain `fly deploy` (no provisor)

A [`fly.toml`](../fly.toml) is included for the flyctl path (provisor does not
read it — it drives the Fly API directly):

```sh
fly launch --no-deploy --copy-config     # or: fly apps create yasdb
fly storage create --name <bucket> --app yasdb   # Tigris bucket + AWS_* secrets
# edit fly.toml: set YASDB_STORE=s3://<bucket>
fly deploy
```

`fly.toml` pins one always-on machine (`auto_stop_machines = off`,
`min_machines_running = 1`), a `GET /__health` check, and a 512 MB guest — the
same shape provisor produces. Never `fly scale count` above 1.

---

## 7. Upgrades, teardown, and the single-writer caveat

```sh
provisor upgrade yasdb --image registry.fly.io/<your-app>:yasdb-2
provisor delete  yasdb
```

Because there is exactly one writer, an image upgrade **replaces** the machine;
expect a brief unavailability window while the new machine boots and opens the
store. Do not raise `--scale` to avoid it — two machines on the same store is a
correctness violation, not a performance tuning knob. If you need higher write
throughput, tune `YASDB_FLUSH` down and rely on yasdb's group commit (see
[`BENCHMARKS.md`](../BENCHMARKS.md)); if you need HA, that requires the multi-node
work noted in [`SPEC.md`](../SPEC.md) §2 (sticky routing + a writer lease), which
provisor's per-stream routing hooks are a natural place to add later.
```
