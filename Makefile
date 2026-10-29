# mdedit build targets. See DESIGN.md.

BIN      := bin/mdedit
WEB_DIR  := web
UNIT     := deploy/mdedit.service
UNIT_DIR := $(HOME)/.config/systemd/user
# The unit runs %h/.local/bin/mdedit; install links it to this checkout's
# binary, so the repo can live anywhere.
BIN_LINK := $(HOME)/.local/bin/mdedit
SERVICE  := mdedit

.PHONY: all web build test install clean

all: web build

web:
	cd $(WEB_DIR) && npm ci && npm run build

build:
	go build -o $(BIN) .

test:
	go test ./...
	@if [ -d $(WEB_DIR)/node_modules ]; then cd $(WEB_DIR) && npm run typecheck; else echo "skipping web typecheck: $(WEB_DIR)/node_modules missing (run make web)"; fi

install: build
	mkdir -p $(UNIT_DIR) $(dir $(BIN_LINK))
	ln -sf $(CURDIR)/$(BIN) $(BIN_LINK)
	ln -sf $(CURDIR)/$(UNIT) $(UNIT_DIR)/$(SERVICE).service
	systemctl --user daemon-reload
	systemctl --user enable --now $(SERVICE)

clean:
	rm -rf bin
