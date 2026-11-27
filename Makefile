GO ?= go
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: build deps test vet fmt tidy tidy-check lint vuln check

build:
	$(GO) build ./...

# Each package builds from its own list: the root from the loop's
# libraries, as summary does, script with agentworkspace added, workflow
# with agentskill and yaml added. agentturn/session is a module of its own, and no
# package's. go list -deps without -test leaves the test dependencies out.
OURS := github.com/ChristopherDavenport
ROOT_DEPS := agentflow|agentpolicy|agenttool|agentturn|openresponses

deps:
	@fail=0; \
	check() { \
	  deps=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' "$$1" \
	    | awk -v allow="^($$2)(/|$$)" '$$0 ~ "^$(OURS)/agentturn/session(/|$$)" || $$0 !~ allow'); \
	  test -z "$$deps" || { echo "$$1 depends on:"; echo "$$deps"; fail=1; }; \
	}; \
	check . '$(OURS)/($(ROOT_DEPS))'; \
	check ./script '$(OURS)/($(ROOT_DEPS)|agentworkspace)'; \
	check ./workflow '$(OURS)/($(ROOT_DEPS)|agentskill)|go.yaml.in/yaml/v3'; \
	check ./summary '$(OURS)/($(ROOT_DEPS))'; \
	check ./examples '$(OURS)/($(ROOT_DEPS))'; \
	exit $$fail

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
