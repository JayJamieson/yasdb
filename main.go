package main

import (
    "io"
    "slatedb.io/slatedb-go"
)

// In memory object storage (development)
db, _ := slatedb.Open("/tmp/cache")
defer db.Close()

// S3 (production)
db, _ := slatedb.Open("/tmp/cache", slatedb.WithUrl[slatedb.DbConfig]("s3://bucket/"))
defer db.Close()

// Environment variables (automatic fallback)
db, _ := slatedb.Open("/tmp/cache", slatedb.WithEnvFile[slatedb.DbConfig]("/path/to/env"))
defer db.Close()

// Basic operations
db.Put([]byte("key"), []byte("value"))
value, _ := db.Get([]byte("key"))

// Range scanning with iterator
iter, _ := db.Scan([]byte("prefix:"), []byte("prefix;"))
defer iter.Close()
for {
    kv, err := iter.Next()
    if err == io.EOF { break }
    // Process kv.Key and kv.Value
}

// Scanning with custom options
opts := &slatedb.ScanOptions{
    DurabilityFilter: slatedb.DurabilityRemote, // Only persistent data
    ReadAheadBytes:   1024,
    MaxFetchTasks:    4, // Higher concurrency
}
iter, _ := db.ScanWithOptions([]byte("prefix:"), []byte("prefix;"), opts)
defer iter.Close()
for {
    kv, err := iter.Next()
    if err == io.EOF { break }
    // Process kv.Key and kv.Value
}
