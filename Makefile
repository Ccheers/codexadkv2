.PHONY: all build test test-race test-integration vet generate sync-schemas check-generated lint clean

GO ?= go

all: generate build test

build:
	$(GO) build ./...

# The default suite is hermetic: it drives a scripted in-process server and needs
# neither the codex binary nor an account.
test:
	$(GO) test ./...

test-race:
	$(GO) test -race -count=2 ./...

# Talks to a real `codex app-server`. Skips itself when codex is not installed.
test-integration:
	$(GO) test -tags integration ./codex/ -v

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./codex/

# Regenerate codex/protocol from the vendored schemas. Output is committed.
generate:
	$(GO) run ./internal/cmd/schemagen
	$(GO) build ./...

# Refresh the vendored schemas from the installed codex CLI.
#
# This is deliberately separate from `generate`: bumping the protocol version
# should be an explicit, reviewable diff rather than something that happens
# silently on someone's machine. Both variants are vendored, because the
# experimental members are only identifiable by diffing them.
sync-schemas:
	@command -v codex >/dev/null 2>&1 || { echo "codex not found in PATH"; exit 1; }
	@rm -rf /tmp/codex-schema-sync
	@mkdir -p /tmp/codex-schema-sync/exp /tmp/codex-schema-sync/stable
	codex app-server generate-json-schema --out /tmp/codex-schema-sync/exp --experimental
	codex app-server generate-json-schema --out /tmp/codex-schema-sync/stable
	@for f in codex_app_server_protocol.schemas.json \
	          codex_app_server_protocol.v2.schemas.json \
	          ClientRequest.json ServerRequest.json \
	          ServerNotification.json ClientNotification.json; do \
		cp /tmp/codex-schema-sync/exp/$$f internal/schemas/$$f; \
		cp /tmp/codex-schema-sync/stable/$$f internal/schemas/stable-$$f; \
	done
	@codex --version | awk '{print $$2}' > internal/schemas/VERSION
	@echo "Vendored schemas for codex-cli $$(cat internal/schemas/VERSION)."
	@echo "Now run 'make generate' and review the diff."

# CI gate: fail if the committed generated code is stale or hand-edited.
check-generated: generate
	@git diff --exit-code -- codex/protocol || { \
		echo ""; \
		echo "codex/protocol is out of date or was edited by hand."; \
		echo "Run 'make generate' and commit the result."; \
		exit 1; \
	}

lint:
	gofmt -l . | tee /dev/stderr | (! read)

clean:
	$(GO) clean ./...
