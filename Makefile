# cfd-client — build & cross-compile.
#
# Static, dependency-free binaries (CGO disabled), so cross-compiling needs no C
# toolchain or target machine — just the Go toolchain:
#   make dist        # -> dist/cfd-client.exe (Windows) + dist/cfd-client-linux-amd64
#   make build       # native binary for the current OS/arch
BIN := cfd-client
LDFLAGS := -s -w          # strip symbol/debug info -> smaller binary
CROSS := CGO_ENABLED=0

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

dist: dist/$(BIN).exe dist/$(BIN)-linux-amd64

dist/$(BIN).exe:
	$(CROSS) GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $@ .

dist/$(BIN)-linux-amd64:
	$(CROSS) GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $@ .

vet:
	go vet ./...

clean:
	rm -f $(BIN)
	rm -rf dist

.PHONY: build dist vet clean
