PLUGIN_ID := cpa-view
VERSION ?= 0.1.0
GO ?= go
DIST_DIR := $(CURDIR)/dist
WEB_DIR := $(CURDIR)/web

.PHONY: web build plugin test verify package clean

web:
	cd $(WEB_DIR) && npm run build

build: plugin

plugin: web
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 $(GO) build -buildvcs=false -buildmode=c-shared -o $(DIST_DIR)/$(PLUGIN_ID).dylib .
	rm -f $(DIST_DIR)/$(PLUGIN_ID).h

test:
	$(GO) test ./...

verify:
	gofmt -l $$(find . -name '*.go' -not -path './web/node_modules/*') | tee /tmp/cpa-view-gofmt
	test ! -s /tmp/cpa-view-gofmt
	$(GO) test ./...
	$(MAKE) plugin

package: plugin
	mkdir -p $(DIST_DIR)/release
	zip -j $(DIST_DIR)/release/$(PLUGIN_ID)-darwin-arm64-v$(VERSION).zip $(DIST_DIR)/$(PLUGIN_ID).dylib manifest.yaml
	shasum -a 256 $(DIST_DIR)/release/$(PLUGIN_ID)-darwin-arm64-v$(VERSION).zip > $(DIST_DIR)/release/$(PLUGIN_ID)-darwin-arm64-v$(VERSION).sha256

clean:
	rm -rf $(DIST_DIR)
