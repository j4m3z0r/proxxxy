.PHONY: all build test clean install

all: build

build:
	go build -o proxxxy-server   ./cmd/server/
	go build -o proxxxy-client   ./cmd/client/
	go build -o proxxxy-testclient ./cmd/testclient/
	go build -o proxxxy-ctl      ./cmd/ctl/
	go build -o proxxxy-xlog     ./cmd/xlog/

test:
	go test ./...

install:
	go install ./...

clean:
	rm -f proxxxy-server proxxxy-client proxxxy-testclient proxxxy-ctl proxxxy-xlog
