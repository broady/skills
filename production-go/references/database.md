# Database & Async Patterns

Load the sub-file relevant to the current task. Skip the rest.

| File | Covers | Load when... |
|---|---|---|
| [database/transactions.md](database/transactions.md) | Explicit tx passing, Querier interface, WithTx helper, nested service calls, transaction rules | Writing or reviewing code that uses SQL transactions |
| [database/cursor-iteration.md](database/cursor-iteration.md) | Keyset pagination, batched processing of large result sets | Iterating over large tables or implementing paginated queries |
| [database/async-brokers.md](database/async-brokers.md) | External broker consumers, retry with backoff, at-least-once delivery, in-process queues, decision table | Implementing async processing, background jobs, or message handling |

For dev-only runtime invariant checks (not database-specific), see [invariant-checks.md](invariant-checks.md).
