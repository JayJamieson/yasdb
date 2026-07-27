# syntax=docker/dockerfile:1
#
# yasdb container image. The build links against the bundled SlateDB native
# library (lib/libslatedb_uniffi.so); the runtime stage carries it and bakes an
# rpath so no LD_LIBRARY_PATH is needed.

FROM golang:1.25-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=1
# rpath points at the runtime location of the .so so the binary is self-locating.
RUN CGO_LDFLAGS="-L/src/lib -Wl,-rpath,/usr/local/lib" go build -trimpath -o /out/yasdb .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/lib/libslatedb_uniffi.so /usr/local/lib/
RUN ldconfig
COPY --from=build /out/yasdb /usr/local/bin/yasdb
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

EXPOSE 4437
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
