# WatchPoint Platform

A serverless weather monitoring and alerting platform built with Go (API/workers) and Python (scientific compute), deployed on AWS via SAM.

The architecture was designed through rigorous flow simulation — every system flow was walked through at a low level before specifications were written. The resulting design documents are thorough and deterministic. Implementers should trust the specifications and follow them closely, flagging genuine issues through the architectural suggestion mechanism rather than making independent architectural decisions.

---

## Project Structure

```
watchpoint/
├── CLAUDE.md                              ← You are here
├── master-prompt.md                       ← Implementation orchestrator (read when asked to execute)
├── implementation_checklist_tree.json      ← Task definitions (13 phases, ~136 tasks)
│
├── .watchpoint/                           ← Execution state (created at runtime)
│   ├── state.json                         ← Progress tracking — source of truth
│   └── reports/
│       ├── error/                         ← Blocker/failure reports from implementer
│       └── architectural/                 ← Architecture gap suggestions from implementer
│
├── .claude/
│   └── agents/
│       └── implementer.md                 ← Sub-agent for task implementation
│
├── architecture/                          ← Design specifications (read-only reference)
│   ├── [spec files]
│   └── [design documents]
│
├── cmd/                                   ← Application entry points
├── internal/                              ← Core packages
├── migrations/                            ← Database migrations
└── runpod/                                ← RunPod inference integration
```

---

## Architecture Directory Guide

All architecture specs live in `architecture/`. These are the authoritative reference for implementation.

### Foundation
- `01-foundation-types.md` — Domain types, interfaces, error codes, DTOs, context helpers, validation logic
- `02-foundation-db.md` — Database schema, migrations, repository pattern, SQL queries
- `03-config.md` — Environment configuration, SecretString, config loading

### Infrastructure
- `04-sam-template.md` — AWS SAM deployment template, Lambda definitions, IAM roles

### API Layer
- `05a-api-core.md` — Router setup, middleware chain, health endpoints
- `05b-api-watchpoints.md` — WatchPoint CRUD, tile assignment, condition management
- `05c-api-forecasts.md` — Forecast retrieval, data access patterns
- `05d-api-organization.md` — Org management, member roles, invitations
- `05e-api-billing.md` — Stripe integration, plan management, usage tracking
- `05f-api-auth.md` — OAuth flows, JWT handling, session management

### Workers
- `06-batcher.md` — S3 event processing, tile-based batching, SQS dispatch
- `07-eval-worker.md` — Python evaluation engine, Zarr reading, threshold evaluation
- `08a-notification-core.md` — Notification routing, deduplication, channel dispatch
- `08b-email-worker.md` — SendGrid integration, template rendering
- `08c-webhook-worker.md` — Webhook delivery, retry logic, SSRF protection

### System
- `09-scheduled-jobs.md` — Cron jobs: archiver, data poller, dead man's switch
- `10-external-integrations.md` — Third-party API contracts
- `11-runpod.md` — RunPod serverless inference integration
- `12-operations.md` — Deployment procedures, monitoring, runbooks

### Setup & Traceability
- `13-human-setup.md` — Manual setup steps, bootstrap protocol
- `99-traceability-matrix.md` — Flow-to-component mapping

### Critical Resources
- `flow-simulations.md` — **The most valuable implementation reference.** Low-level simulated walkthroughs of every system flow. This is how the architecture was validated and is the best resource for understanding how components interact at runtime.
- `flow-simulation-standard.md` — Standard format for flow simulations
- `architectural-impact-report-of-flow-simulations.md` — Changes derived from simulation findings

### Design Origins
- `watchpoint-platform-design-final.md` — Original platform design
- `watchpoint-design-addendum-v3.md` — Design refinements
- `watchpoint-tech-stack-v3.3.md` — Technology decisions and project structure
- `watchpoint-flows-v1.md` — Flow definitions (higher-level than simulations)
- `watchpoint-architecture-plan-v1.md` — Architecture plan and sub-document inventory

---

## Implementation Process

The implementation is driven by `master-prompt.md` using the `implementer` sub-agent.

**To start or resume**: Tell Claude to execute the master prompt. It will read `master-prompt.md`, initialize or resume from `.watchpoint/state.json`, and begin processing tasks sequentially.

**State is persistent**: Progress is tracked in `.watchpoint/state.json`. The process can be interrupted and resumed at any time. On resumption, the orchestrator picks up from where it left off.

**Do not modify** the architecture files unless explicitly agreed upon during an architectural suggestion review.

---

## Orchestrator Rules

### Git Commits
After each completed **subitem** (task), the orchestrator must stage and commit the changes with a descriptive commit message. Do not wait until the end of a phase — commit after every individual task completion.

### Phase Boundaries
By default, the orchestrator must **pause at the end of each phase** and wait for the human to say when to continue. Do not automatically proceed to the next phase unless the human has explicitly said to keep going (e.g., "keep going until the next HUMAN task" or "continue through Phase X"). If no such instruction is active, always stop and ask.