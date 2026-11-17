# The device agent is cross-compiled for every Android ABI and embedded into
# the host binary, which installs the right one on first contact.
AGENT_DIR := internal/agentbin/bin
LDFLAGS   := -s -w
ARCHS     := arm arm64 386 amd64

.PHONY: all agents build build-nomtp app dist dmg test clean

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
	xcrun actool gui/Resources/AppIcon.icon --compile bin/MacDroidFS.app/Contents/Resources \
		--app-icon AppIcon --platform macosx --minimum-deployment-target 15.0 \
		--output-partial-info-plist gui/.build/AppIcon-partial.plist >/dev/null
	codesign --force --sign - bin/MacDroidFS.app/Contents/MacOS/macdroidfsd
	codesign --force --sign - bin/MacDroidFS.app

# ---- Release: a universal (arm64 + x86_64) app in a drag-to-Applications DMG.
#
#   make dmg                             sign with the Developer ID Application
#                                        certificate found in the keychain
#   make dmg NOTARY_PROFILE=<profile>    also notarize and staple
#   make dmg SIGN_ID=-                   ad hoc, without a certificate
#
# libusb is built from source as a universal dylib and shipped inside the app
# (Contents/Frameworks), so the target Mac needs no Homebrew. The notary profile
# is created once with `xcrun notarytool store-credentials <profile>`.
VERSION        := $(shell /usr/libexec/PlistBuddy -c 'Print CFBundleShortVersionString' gui/Resources/Info.plist)
MIN_MACOS      := 15.0
DIST           := bin/dist
DIST_APP       := $(DIST)/MacDroidFS.app
DMG            := $(DIST)/MacDroidFS-$(VERSION).dmg
LIBUSB_PREFIX  := $(abspath $(DIST))/libusb
LIBUSB_DYLIB   := $(LIBUSB_PREFIX)/lib/libusb-1.0.0.dylib
SIGN_ID        ?= $(shell security find-identity -v -p codesigning | sed -n 's/.*"\(Developer ID Application:[^"]*\)".*/\1/p' | head -1)
NOTARY_PROFILE ?=
SWIFT_ARCHS    := --arch arm64 --arch x86_64

# Notarization requires the hardened runtime and a secure timestamp. An ad hoc
# signature gets neither: library validation would then reject the bundled
# libusb, which has no team ID either.
ifeq ($(SIGN_ID),-)
CODESIGN := codesign --force --sign -
else
CODESIGN := codesign --force --options runtime --timestamp --sign "$(SIGN_ID)"
endif

$(LIBUSB_DYLIB):
	rm -rf $(DIST)/libusb-src
	mkdir -p $(DIST)/libusb-src
	brew fetch -s libusb
	tar -xjf "$$(brew --cache -s libusb)" -C $(DIST)/libusb-src --strip-components 1
	cd $(DIST)/libusb-src && ./configure --prefix=$(LIBUSB_PREFIX) \
		--disable-static --disable-dependency-tracking \
		CFLAGS='-O2 -arch arm64 -arch x86_64 -mmacosx-version-min=$(MIN_MACOS)' \
		LDFLAGS='-arch arm64 -arch x86_64 -mmacosx-version-min=$(MIN_MACOS)' >/dev/null
	$(MAKE) -C $(DIST)/libusb-src install >/dev/null
	install_name_tool -id @executable_path/../Frameworks/libusb-1.0.0.dylib $@

# The daemon, linked against the bundled libusb, once per architecture.
$(DIST)/macdroidfsd: agents $(LIBUSB_DYLIB)
	@for a in arm64:arm64 amd64:x86_64; do \
		goarch=$${a%%:*}; carch=$${a##*:}; echo "macdroidfsd darwin/$$goarch"; \
		CGO_ENABLED=1 GOOS=darwin GOARCH=$$goarch \
		CC="clang -arch $$carch -mmacosx-version-min=$(MIN_MACOS)" \
		PKG_CONFIG_LIBDIR=$(LIBUSB_PREFIX)/lib/pkgconfig PKG_CONFIG_PATH= \
			go build -trimpath -ldflags '$(LDFLAGS)' -o $@-$$goarch ./cmd/macdroidfs || exit 1; \
	done
	lipo -create -output $@ $@-arm64 $@-amd64

dist: $(DIST)/macdroidfsd
	@test -n "$(SIGN_ID)" || { echo "no Developer ID Application certificate in the keychain; pass SIGN_ID=- for ad hoc"; exit 1; }
	cd gui && swift build -c release $(SWIFT_ARCHS)
	rm -rf $(DIST_APP)
	mkdir -p $(DIST_APP)/Contents/MacOS $(DIST_APP)/Contents/Resources $(DIST_APP)/Contents/Frameworks
	cp gui/Resources/Info.plist $(DIST_APP)/Contents/
	cp "$$(cd gui && swift build -c release $(SWIFT_ARCHS) --show-bin-path)/MacDroidFS" $(DIST_APP)/Contents/MacOS/
	cp $(DIST)/macdroidfsd $(DIST_APP)/Contents/MacOS/macdroidfsd
	cp $(LIBUSB_DYLIB) $(DIST_APP)/Contents/Frameworks/
	xcrun xcstringstool compile gui/Resources/Localizable.xcstrings --output-directory $(DIST_APP)/Contents/Resources
	xcrun actool gui/Resources/AppIcon.icon --compile $(DIST_APP)/Contents/Resources \
		--app-icon AppIcon --platform macosx --minimum-deployment-target $(MIN_MACOS) \
		--output-partial-info-plist gui/.build/AppIcon-partial.plist >/dev/null
	$(CODESIGN) $(DIST_APP)/Contents/Frameworks/libusb-1.0.0.dylib
	$(CODESIGN) $(DIST_APP)/Contents/MacOS/macdroidfsd
	$(CODESIGN) $(DIST_APP)
	codesign --verify --strict --deep $(DIST_APP)

dmg: dist
	rm -rf $(DIST)/dmg $(DMG)
	mkdir -p $(DIST)/dmg
	cp -R $(DIST_APP) $(DIST)/dmg/
	ln -s /Applications $(DIST)/dmg/Applications
	hdiutil create -volname "MacDroidFS $(VERSION)" -srcfolder $(DIST)/dmg -fs HFS+ -format UDZO -ov $(DMG) >/dev/null
	rm -rf $(DIST)/dmg
ifneq ($(SIGN_ID),-)
	codesign --force --timestamp --sign "$(SIGN_ID)" $(DMG)
endif
ifneq ($(NOTARY_PROFILE),)
	xcrun notarytool submit $(DMG) --keychain-profile "$(NOTARY_PROFILE)" --wait
	xcrun stapler staple $(DMG)
endif
	@echo $(DMG)

test: agents
	go vet ./...
	go test ./...

clean:
	rm -rf bin gui/.build $(AGENT_DIR)/macdroidfs-agent-*
