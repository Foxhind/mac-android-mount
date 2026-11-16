# The device agent is cross-compiled for every Android ABI and embedded into
# the host binary, which installs the right one on first contact.
AGENT_DIR := internal/agentbin/bin
LDFLAGS   := -s -w
ARCHS     := arm arm64 386 amd64

.PHONY: all agents build build-nomtp test clean

all: build

agents:
	@for a in $(ARCHS); do \
		echo "agent linux/$$a"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$a GOARM=7 \
			go build -trimpath -ldflags '$(LDFLAGS)' -o $(AGENT_DIR)/droidfs-agent-$$a ./cmd/droidfs-agent || exit 1; \
	done

# MTP mode links libusb through cgo (brew install libusb pkg-config).
build: agents
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/droidfs ./cmd/droidfs

# adb-only build without cgo or libusb.
build-nomtp: agents
	CGO_ENABLED=0 go build -tags nomtp -trimpath -ldflags '$(LDFLAGS)' -o bin/droidfs ./cmd/droidfs

test: agents
	go vet ./...
	go test ./...

clean:
	rm -rf bin $(AGENT_DIR)/droidfs-agent-*
