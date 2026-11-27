# Contributing

Issues and pull requests are welcome.

## Before you start

This library drives the loop through a graph. It builds on the public
surface of `agentturn`: a change that needs the loop to do something new
belongs there, and the loop learns nothing about workflows. A step's
policy is `agentpolicy`'s grammar and engine, so a change to how rules
match belongs there too.

The design is in `docs/design/`, one document per topic, starting at
`overview.md`. Read the documents your change touches, and open an issue
first for anything larger than a bug fix. A change of design is a pull
request that edits those documents: they describe the design as it is,
`decisions.md` holds why, and `open-questions.md` holds what is unbuilt,
unsettled or untried.

## Development

Go 1.26 or later is required. The tests run shell scripts that need `jq`
and `git`. The full local check is:

```sh
make check        # gofmt, tidy, vet, deps, staticcheck, govulncheck, race tests
```

Each package builds from its own short list of dependencies; `make deps`
fails if anything else creeps in.

## Pull requests

- Keep the change focused; unrelated cleanups belong in their own PR.
- Add or update tests. Tests run offline against scripted models.
- A change to what a model reads, such as instructions, reasons or
  questions, is a user-visible change. The trials in
  `examples/live_test.go` are how it is tried against a real model; say
  in the pull request whether you ran them.
- Run `make check` before pushing. CI runs the same steps on the minimum
  and current Go versions.
- Note user-visible changes under *Unreleased* in `CHANGELOG.md`.

## Releases

With the changelog's *Unreleased* section written:

```sh
make release VERSION=vX.Y.Z
```

dates the changelog, runs `make check`, commits, guards and tags the
version with the changelog section as the message, and pushes the branch
and the tag in one atomic push. The release workflow publishes a GitHub
release per tag, and the Go module proxy picks the version up. Before
v1.0.0 the API may change between minor versions; the changelog records
every break.

`make release-guard TAG=<tag>` refuses a dirty tree, a tag that already
exists locally or on origin, and a version that does not sort above the
current release, since the proxy and the checksum database keep every
published version forever. `make release` runs it after the release
commit and before the tag, so a refusal costs a `git reset --hard
HEAD~1`.
