GO ?= go
TAGS := xz,zstd,lz4
FIXTURES ?= $(CURDIR)/.fixtures

.PHONY: build test fixtures

build:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -tags $(TAGS) -o backup-mounter.exe .

test:
	BM_FIXTURES=$(FIXTURES) $(GO) test -tags $(TAGS) ./...

fixtures:
	testdata/make_fixtures.sh $(FIXTURES)
