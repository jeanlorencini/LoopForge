# Architecture

The platform keeps the upstream Go module name `agentflow`, package boundaries and `AGENTFLOW_` environment variables to reduce ongoing merge conflicts.

| Directory | Responsibility |
| --- | --- |
| `cmd/agentflow` | API, worker or combined process |
| `cmd/agentflow-runner` | Personal and pool execution machines |
| `cmd/agentflow-mcp`, `cmd/agentflow-gate-mcp` | Public MCP and structured gate results |
| `internal/engine` | SOP graph, leases, scheduling, sandbox lifecycle and delivery. Ready branches execute together in the run workspace; rerunning one branch after a cancel restores cancelled siblings |
| `internal/repo` | MySQL persistence, one repository per business domain |
| `internal/httpapi` | HTTP/MCP handlers, one handler per business domain |
| `internal/runtime`, `internal/runner` | Executor protocols, credentials, isolation and artifacts |
| `internal/tracker` | Remote issue/PR adapters |
| `web` | Embedded upstream web interface, preferences and public sign-in |
| `migrations` | Explicit data migrations and optional public catalog |
| `local` | Local workflows and host integrations; user guides live in `docs/local/` |

The public edition adds account/password login and GitHub adapters. These feed the existing user, token, repository and delivery contracts; there is no separate public engine or in-memory production store. A remote repository identity beginning `github:` distinguishes GitHub owner/repository names from legacy tracker identities. The upstream `gongfengProjectId` JSON field carries this identity for compatibility.

The CLI's Classic and Portable workflow assets remain separate from each other and from platform role definitions. Sharing behavior contracts is preferable to copying runtime assets. No classic host bundle is required by a platform worker.

## Run recovery and observability

The engine applies retry policy to an observed process exit code. A remote runner keeps its completion report while awaiting acknowledgement. The API accepts an identical retry for the same authorized attempt, and a queued task tied to an unavailable runner ends without automatic retry.

Run recovery selects eligible IDs in RunsRepo and distinguishes a missing run from a failed read. Delivery persists outbox diagnostics with its status. The live trace merges streamed events with paginated results and fetches missing events when a run reaches a terminal state.

The Docker runtime parks shared containers for later reuse after validating their binding. Explicit cleanup removes them. SOP replay checks prerequisites before running and binds review approval to the Git tree so a tree change can send the workflow back to review.

Recovery reconciles runner work using the run, node, attempt and operation. The run lease and revision protect execution transitions, and replay preserves pause and cancellation controls. SOP checkpoints carry recovery proofs in writable state outside the delivered tree. Delivery recovery reconciles the same pull request using the recorded Git revisions.
