# yasdb — Durable Streams over SlateDB
LIBDIR := $(CURDIR)/lib
export CGO_ENABLED := 1
export CGO_LDFLAGS := -L$(LIBDIR) -Wl,-rpath,$(LIBDIR)
export LD_LIBRARY_PATH := $(LIBDIR)

.PHONY: build test bench fuzz run tidy maelstrom maelstrom-chaos
build:
	go build ./...
test:
	go test ./... $(ARGS)
bench:
	go test -run='^$$' -bench=. -benchmem -benchtime=2s ./internal/ds/ $(ARGS)
fuzz:
	go test ./internal/ds/ -run='^$$' -fuzz=FuzzLiveCacheMatchesStore $(ARGS)
run: build
	go run . $(ARGS)
tidy:
	go mod tidy
maelstrom:
	MAELSTROM=$(MAELSTROM) READ=$(READ) test/maelstrom/run.sh $(DUR)
maelstrom-chaos:
	MAELSTROM=$(MAELSTROM) test/maelstrom/run-nemesis.sh $(DUR)
