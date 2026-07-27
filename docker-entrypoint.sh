#!/bin/sh
# Maps environment variables to yasdb flags. Two storage modes:
#   - object store:  set YASDB_STORE=s3://<bucket> (+ AWS_* env for Tigris/S3)
#   - local volume:  leave YASDB_STORE empty; data goes to YASDB_DATA (a mount)
set -eu

# Bridge provisor-injected secrets (PROVISOR_SECRET_<NAME>) to their bare names
# when unset, so e.g. PROVISOR_SECRET_AWS_ACCESS_KEY_ID -> AWS_ACCESS_KEY_ID.
for _v in $(env | sed -n 's/^\(PROVISOR_SECRET_[A-Za-z0-9_]*\)=.*/\1/p'); do
    _name=${_v#PROVISOR_SECRET_}
    eval "_cur=\${$_name:-}"
    if [ -z "$_cur" ]; then eval "export $_name=\"\${$_v}\""; fi
done

ADDR=":${PORT:-4437}"
FLUSH="${YASDB_FLUSH:-10ms}"

if [ -n "${YASDB_STORE:-}" ]; then
    exec yasdb -addr "$ADDR" -store "$YASDB_STORE" -db "${YASDB_DB:-yasdb}" -flush "$FLUSH"
else
    exec yasdb -addr "$ADDR" -data "${YASDB_DATA:-/data/yasdb}" -flush "$FLUSH"
fi
