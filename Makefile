# The device agent is cross-compiled for every Android ABI and embedded into
# the host binary, which installs the right one on first contact.
AGENT_DIR := internal/agentbin/bin
LDFLAGS   := -s -w
ARCHS     := arm arm64 386 amd64

.PHONY: all agents build build-nomtp app test clean

all: build

agents:
	@for a in $(ARCHS); do \
		echo "agent linux/$$a"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$a GOARM=7 \
			go build -trimpath -ldflags '$(LDFLAGS)' -o $(AGENT_DIR)/macdroidfs-agent-$$a ./cmd/macdroidfs-agent || exit 1; \
	done

# MTP mode links libusb through cgo (brew install libusb pkg-config).
build: agents
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/macdroidfs ./cmd/macdroidfs

# adb-only build without cgo or libusb.
build-nomtp: agents
	CGO_ENABLED=0 go build -tags nomtp -trimpath -ldflags '$(LDFLAGS)' -o bin/macdroidfs ./cmd/macdroidfs

# The MacDroidFS app (SwiftUI) with the daemon inside: bin/MacDroidFS.app.
# Needs Xcode. The bundle is signed ad hoc, for this Mac only. The daemon is
# renamed macdroidfsd inside it: APFS is case-insensitive, and "macdroidfs" would
# overwrite the app's own "MacDroidFS" executable.
app: build
	cd gui && swift build -c release
	rm -rf bin/MacDroidFS.app
	mkdir -p bin/MacDroidFS.app/Contents/MacOS bin/MacDroidFS.app/Contents/Resources
	cp gui/Resources/Info.plist bin/MacDroidFS.app/Contents/
	cp "$$(cd gui && swift build -c release --show-bin-path)/MacDroidFS" bin/MacDroidFS.app/Contents/MacOS/
	cp bin/macdroidfs bin/MacDroidFS.app/Contents/MacOS/macdroidfsd
	xcrun xcstringstool compile gui/Resources/Localizable.xcstrings --output-directory bin/MacDroidFS.app/Contents/Resources
	codesign --force --sign - bin/MacDroidFS.app/Contents/MacOS/macdroidfsd
	codesign --force --sign - bin/MacDroidFS.app

test: agents
	go vet ./...
	go test ./...

clean:
	rm -rf bin gui/.build $(AGENT_DIR)/macdroidfs-agent-*
