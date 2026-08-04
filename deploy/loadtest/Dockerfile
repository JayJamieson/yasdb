# In-region load generator for yasdb: vegeta + benchmark/loadshape.
#
# Runs in the SAME Fly org + region as the target app, so it reaches yasdb over
# 6PN private networking (http://yasdb.internal:4437) — bypassing the public
# Anycast edge, TLS, and the proxy/concurrency limits. You measure the database,
# not the internet.
#
# Build context is the repo ROOT (so it can COPY benchmark/). Deploy with:
#   fly deploy -c deploy/loadtest/fly.toml     (run from the repo root)
FROM golang:1.25-alpine AS build
RUN go install github.com/tsenart/vegeta@latest
COPY benchmark/loadshape /loadshape-src
RUN cd /loadshape-src && go build -o /out/loadshape .

FROM alpine:3.20
# bash for the benchmark scripts; curl for connectivity checks; python3 for
# vegeta target-file generation (see benchmark/lib.sh).
RUN apk add --no-cache bash curl python3
COPY --from=build /go/bin/vegeta /usr/local/bin/vegeta
COPY --from=build /out/loadshape /usr/local/bin/loadshape
COPY benchmark /benchmark
WORKDIR /benchmark
# Keep the machine alive so you can `fly ssh console` in and run benchmarks repeatedly.
ENTRYPOINT ["sleep", "infinity"]
