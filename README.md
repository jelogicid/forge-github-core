# forge — a standalone GitHub-compatible git forge

One process, one in-memory store, four surfaces an agent already speaks:

- **git smart HTTP** at `/{owner}/{repo}.git` — upload-pack protocol v2 (ls-refs,
  fetch, shallow, sideband-64k) and receive-pack v0 (report-status, delete-refs,
  fast-forward enforcement).
- **REST v3** under `/api/v3` — repositories, contents, labels, issues, pulls,
  merges, commits, refs, pagination.
- **GraphQL v4** at `/api/graphql` — viewer/repository/node, issue & pull-request
  queries and mutations, Relay connections, introspection, batched operations.
- **MCP** at `/mcp` — JSON-RPC 2.0 Streamable HTTP, revision 2026-07-28, the
  github-mcp-server tool surface over the same store.

No UI, no database, no git binary, no outside service. Everything it serves is
its own.

## Layout

- `main.py` — launcher the pod runs (`python3 -P -s /bot/main.py`); hands the
  process to the vendored static binary.
- `forge` — statically linked Linux amd64 binary (Go, `CGO_ENABLED=0`), committed
  as vendored source.
- `*.go` — the source the binary is built from: `main.go` (lifecycle),
  `store.go` (state), `gitobj.go`/`pack.go` (git objects + pack wire),
  `githttp.go` (smart HTTP), `rest.go` (REST v3), `graphql.go` (GraphQL),
  `mcp.go` (MCP).

## Build

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o forge .
```

## Lifecycle

Binds an ephemeral port on `0.0.0.0`, POSTs `{"port": N}` to
`$MAKEFUN_ENV_URL/register`, serves the suite, long-polls `$MAKEFUN_ENV_URL/done`
and exits 0 when the trial is over. Without `MAKEFUN_ENV_URL` it serves on
`FORGE_PORT` (default 8090) for the kit's local runner.

## License

MIT — see `LICENSE`.