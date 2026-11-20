# Contributing

## Build and test

```bash
go build ./...
go test ./...
```

Go 1.23 or newer. The tests run against a fake engine, so no GPU and no container runtime are
needed.

## Commits

Conventional Commits, one logical change per commit. Keep the message in English.

## Pull requests

Open an issue first for anything larger than a fix. Every pull request needs one approval; `main`
is protected. Include how a reviewer can verify the change.

## Scope

cardo manages which engine occupies a GPU slot. It does not install engines, download models, or
know anything about a specific inference server. If a proposal needs a server-specific detail, it
belongs in the engine config file, not in the code.
