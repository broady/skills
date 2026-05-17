# Project Layout

Standard layout for a production Go service. Adapt to the project's needs;
do not force this structure onto existing codebases.

```
cmd/server/main.go        # flags, wiring, signal handling
internal/
  domain/                  # core types, zero external deps
  store/                   # data access (implements Querier-based interfaces)
  service/                 # business logic, orchestration
  transport/http/          # HTTP handlers (thin: decode -> call -> encode)
  transport/grpc/          # gRPC/Connect handlers
  middleware/              # auth, logging, metrics
proto/                     # protobuf definitions
migrations/                # DB migrations (sequential, numbered)
Taskfile.yml               # build, test, lint, run
.golangci.yml              # linter config
```

## Principles

- **`cmd/`** — one binary per subdirectory. Only `main.go` (and maybe a
  `main_test.go` for smoke tests). Construction, signal handling, `os.Exit`.
- **`internal/`** — unexportable implementation. Prevents external imports.
- **`domain/`** (or `model/`) — pure types and interfaces with zero
  dependencies on infrastructure. Other packages depend inward on domain.
- **`store/`** — data access layer. Accepts `Querier` interface (satisfied by
  both `*sql.DB` and `*sql.Tx`). Returns domain types.
- **`service/`** — orchestration. Owns transaction boundaries. Calls stores.
  Returns domain errors.
- **`transport/`** — thin adapter layer. Decodes wire format, calls service,
  encodes response, maps errors to status codes.
- **Top-level `proto/`** — protobuf source of truth. Generated code goes
  wherever `buf.gen.yaml` or `protoc` config places it (commonly `gen/`).

## When NOT to use this layout

- **Libraries** — flat package, no `cmd/`, no `internal/` unless the library
  is large enough to have true implementation details.
- **Small CLIs** — single `main.go` + a few packages is fine.
- **Existing projects** — preserve the current layout. Do not restructure
  stable code for layout compliance.

## Dependency Direction

```
cmd/server
    |
    +---> transport (HTTP/gRPC)  ---> service  ---> store  ---> domain
```

`cmd/server` wires all components. `transport` imports `service` (calls it).
`service` imports `store` (calls it). `store` imports `domain` (returns its
types). Inner layers never import outer layers. `domain` has zero imports from
other internal packages. `transport` knows about `service` but not `store`.
