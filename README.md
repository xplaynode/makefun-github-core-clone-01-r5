# lucky-forge

A small GitHub-core clone in a single Go binary. It serves, from one
in-memory store:

- **REST v3** — repositories, branches, refs, contents, labels, issues,
  comments and pull requests (including files diff and squash/merge).
- **GraphQL v4** — viewer, repository, issue and pull-request queries with
  cursor connections, `node(id:)`, introspection, and the create/close/
  merge/comment mutations.
- **Git smart HTTP** — protocol v2 (`ls-refs`, `fetch`) and v0 fetch,
  v0 receive-pack with report-status, packfiles built in-process.
- **MCP** — a Model Context Protocol endpoint (`server/discover`,
  `tools/list`, `tools/call`) with six repository tools.

There is no database and no configuration: state lives in memory, object
streams are compressed with a byte-exact deflate implementation so packs
match the bytes other git tooling produces, and hot paths (issue reads,
advertisements, pack building) reuse cached serialisations.

## Running

The harness starts the entry with Node:

```
node main.ts
```

`main.ts` launches the bundled `forge` binary, which binds an ephemeral
port, registers it with `$MAKEFUN_ENV_URL`, serves until the environment
reports done, and exits 0. `MAKEFUN_SHOP_TOKEN` is the bearer token the
API expects.

## Building

```
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o forge .
```

Requires Go 1.27+. The binary is statically linked and has no runtime
dependencies.
