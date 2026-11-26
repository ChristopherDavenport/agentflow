GO ?= go
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: build deps test vet fmt tidy tidy-check lint vuln check

build:
	$(GO) build ./...

# go list -deps without -test leaves the test dependencies out.
deps:
	@deps=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... \
	  | grep -v '^go.yaml.in/yaml/v3$$' \
	  | grep -Ev '^github.com/ChristopherDavenport/(agentflow|agentskill|agenttool|agentturn|agentworkspace|openresponses)(/|$$)' || true); \
	  test -z "$$deps" || { echo "module depends on: $$deps"; exit 1; }

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

tidy-check:
	$(GO) mod tidy -diff

fmt:
	gofmt -l . && test -z "$$(gofmt -l .)"

lint:
	$(STATICCHECK) ./...

vuln:
	$(GOVULNCHECK) ./...

check: fmt tidy-check vet deps lint vuln test
