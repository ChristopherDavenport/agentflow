GO ?= go
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: build deps test vet fmt tidy tidy-check lint vuln check \
	release-guard release clean

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

# Everything CI runs.
check: fmt tidy-check vet deps lint vuln test

# Checks one tag is safe to push, before it is pushed. A pushed tag is
# permanent, since the proxy and the checksum database keep the version
# forever:
#   make release-guard TAG=v0.0.1
release-guard:
	@test -n "$(TAG)" || { echo "usage: make release-guard TAG=<tag>"; exit 1; }
	@scripts/release-guard.sh "$(TAG)"

# Cut a release: date the changelog's Unreleased section, check, commit,
# guard, tag with that section as the message, and push the branch and
# the tag with --atomic, so no tag is visible without its commit.
# TRAILER, when set, is appended to the commit message. The guard runs
# while everything is still local: if it refuses, undo with
# git reset --hard HEAD~1. The changelog is dated through a temp file
# because sed -i differs between GNU and BSD.
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z"; exit 1; }
	@grep -q '^## Unreleased$$' CHANGELOG.md || { echo "CHANGELOG.md has no Unreleased section"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean"; exit 1; }
	sed 's/^## Unreleased$$/## $(VERSION) - '"$$(date +%F)"'/' CHANGELOG.md > CHANGELOG.md.tmp \
	  && mv CHANGELOG.md.tmp CHANGELOG.md \
	  || { rm -f CHANGELOG.md.tmp; exit 1; }
	$(MAKE) tidy
	$(MAKE) check
	git add -A && git commit -q -m "Release $(VERSION)" $(if $(TRAILER),-m "$(TRAILER)")
	@scripts/release-guard.sh "$(VERSION)"
	@notes="$$(scripts/release-notes.sh $(VERSION))" || exit 1; \
	 git tag -a $(VERSION) -m "$$notes"
	git push origin --atomic HEAD $(VERSION)

clean:
	rm -rf .cache
