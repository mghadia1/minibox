# MiniBox needs Linux. On a Mac: `make vm` once, then `make vm-test` and
# `make smoke` run everything in the Lima VM (lima/minibox.yaml).
VM ?= minibox
DEV ?= $(HOME)/.cache/minibox-dev

.PHONY: build build-linux test lint vm vm-test vm-shell vm-stop rootfs smoke clean

# For this machine. On a Mac the binary only says it needs Linux.
build:
	go build -o bin/minibox ./cmd/minibox

# For the VM (arm64 on Apple Silicon): a static binary in the shared cache.
build-linux:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(DEV)/bin/minibox ./cmd/minibox

# What runs here: on a Mac, the tests that do not need Linux.
test:
	go test -race -count=1 ./...

lint:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	GOOS=linux go vet ./...

# Creates the VM the first time (a 620 MB Ubuntu image, then Go and gcc
# inside it), and starts it.
vm:
	mkdir -p $(DEV)
	limactl list --quiet | grep -qx $(VM) || limactl create --tty=false --name=$(VM) lima/minibox.yaml
	limactl start --tty=false $(VM)

# Everything, in the VM, as root: gofmt, go vet, go test -race.
vm-test:
	./scripts/vm-test.sh

vm-shell:
	limactl shell $(VM)

vm-stop:
	limactl stop $(VM)

rootfs:
	./scripts/rootfs.sh aarch64

smoke:
	./scripts/smoke.sh

clean:
	rm -rf bin
