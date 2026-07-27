package main

import (
	_ "embed"
	"net/http"
)

//go:embed ui.html
var uiHTML []byte

// serveUI serves the self-contained demo playground. It is mounted at /__ui/
// and lets you create, append to, read, tail (SSE/long-poll), and delete
// streams against this server.
func serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(uiHTML)
}
