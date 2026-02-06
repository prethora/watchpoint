# WatchPoint Platform — Architecture Plan

> Master plan for the architecture document suite. Defines all sub-documents, their contents, dependencies, and build order.

**Version**: 1.0  
**Date**: 2026-02-01  
**Status**: Approved for implementation  

**Source Documents**:
- `watchpoint-platform-design-final.md`
- `watchpoint-design-addendum-v3.md`
- `watchpoint-tech-stack-v3_3.md`
- `watchpoint-flows-v1.md`

**Generated From**: 3-Question Expansion Analysis (24 questions, 24 answers)

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Architecture Document Goals](#2-architecture-document-goals)
3. [Organizing Principles](#3-organizing-principles)
4. [Technology Selections](#4-technology-selections)
5. [Sub-Document Inventory](#5-sub-document-inventory)
6. [Dependency Graph](#6-dependency-graph)
7. [Build Order](#7-build-order)
8. [Sub-Document Specifications](#8-sub-document-specifications)
9. [Cross-Cutting Concerns](#9-cross-cutting-concerns)
10. [Flow Coverage Summary](#10-flow-coverage-summary)
11. [Completeness Checklist](#11-completeness-checklist)
12. [Document Conventions](#12-document-conventions)
13. [Final Assembly Process](#13-final-assembly-process)

---

## 1. Executive Summary

This document defines the complete plan for building the WatchPoint Platform Architecture Document Suite. The architecture documents will provide AI implementers with everything needed to build the platform without ambiguity — every function signature, every type definition, every dependency relationship — stopping just short of actual code implementation.

### What We're Building

A suite of **19 sub-documents** organized by layer:
- **Foundation Layer** (3 docs): Core types, database schema, configuration
- **Infrastructure Layer** (1 doc): SAM template and AWS resources
- **API Layer** (6 docs): HTTP handlers organized by domain
- **Worker Layer** (5 docs): Batcher, eval worker, notification workers
- **Background Jobs Layer** (1 doc): All scheduled jobs
- **External Systems Layer** (2 docs): Third-party integrations, RunPod inference
- **Operations Layer** (2 docs): Deployment, human setup guide
- **Appendices** (1 doc): Traceability matrix

### Why Sub-Documents

Context window limitations require breaking the architecture into manageable pieces. Each sub-document:
- Fits within a single implementation session
- Has clear boundaries and dependencies
- Can be built and validated independently
- Combines into a complete architecture document when finished

### Target Audience

The architecture documents are written for **AI implementation agents** who will:
- Read the specifications
- Generate working code
- Prompt humans for setup tasks they cannot perform
- Validate implementation against flow specifications

---

## 2. Architecture Document Goals

### What "Complete" Means

An architecture sub-document is complete when an AI implementer can write code without guessing. This requires:

| Element | Required | Notes |
|---------|----------|-------|
| Package declarations | ✓ | `package handlers` |
| Import lists | ✓ | External dependencies named |
| Type/struct definitions | ✓ | All fields, types, JSON tags, validation tags |
| Interface definitions | ✓ | Method signatures with doc comments |
| Function signatures | ✓ | Parameters, returns, purpose, error cases |
| Error handling | ✓ | Which errors returned, error codes |
| Database queries | ✓ | SQL for complex queries |
| Dependencies | ✓ | What this package imports |
| Dependents | ✓ | What imports this package |
| Flow coverage | ✓ | Which flows this component implements |

### What We Explicitly Exclude

| Excluded | Reason |
|----------|--------|
| Function body implementations | That's the implementer's job |
| Test code | Derived from specifications |
| CI/CD pipeline details | Separate concern |
| Trivial SQL (basic CRUD) | Implied by repository interface |
| Obvious comments | Adds noise without value |

### Success Criteria

The architecture document suite is successful when:
1. Every flow from `watchpoint-flows-v1.md` has traceable implementation
2. No circular dependencies exist between packages
3. An AI agent can implement any sub-document without clarifying questions
4. Human setup requirements are explicit and actionable

---

## 3. Organizing Principles

### 3.1 Hybrid Layer + Package Organization

We organize by **layer** (foundation, infrastructure, API, workers, etc.) with **package-level granularity** within layers. This provides:
- Clear dependency direction (lower layers don't depend on higher)
- Cohesive groupings (related code together)
- Manageable document sizes

### 3.2 Foundation First

Foundation documents (01, 02, 03) define types, interfaces, and patterns referenced everywhere. They must be complete before any other documents can be built.

### 3.3 SAM Template as Infrastructure Truth

The SAM template document (04) is the source of truth for:
- What Lambda functions exist
- What queues, buckets, and resources exist
- What environment variables are available
- What IAM permissions are granted

Other documents reference the SAM template for deployment configuration.

### 3.4 Three-Layer Traceability

Every flow is traceable through three layers:

1. **Function comments**: Flow IDs in doc comments
   ```go
   // Create handles POST /v1/watchpoints
   // Flows: WPLC-001 (Event Mode), WPLC-002 (Monitor Mode)
   func (h *WatchPointHandler) Create(...)
   ```

2. **Sub-document tables**: Flow coverage section in each doc
   ```markdown
   ## Flow Coverage
   | Flow ID | Flow Name | Implementation |
   |---------|-----------|----------------|
   | WPLC-001 | Create (Event Mode) | WatchPointHandler.Create |
   ```

3. **Master matrix**: Complete mapping in `99-traceability-matrix.md`

### 3.5 Cross-Reference Pattern

References between documents use consistent format:
- Within same doc: `See [Section 3.2](#32-repository-interfaces)`
- To another doc: `See 02-foundation-db.md Section 4`
- To flows doc: `Implements flow WPLC-001 (see watchpoint-flows-v1.md)`

---

## 4. Technology Selections

### 4.1 Go Dependencies

| Purpose | Package | Version | Rationale |
|---------|---------|---------|-----------|
| HTTP Router | `github.com/go-chi/chi/v5` | v5.x | Lightweight, idiomatic, excellent middleware support |
| DB Driver | `github.com/jackc/pgx/v5` | v5.x | Native Go, connection pooling, better performance than lib/pq |
| Migrations | `github.com/golang-migrate/migrate/v4` | v4.x | File-based, CLI + library, industry standard |
| Validation | `github.com/go-playground/validator/v10` | v10.x | Struct tags, comprehensive validation rules |
| Logging | `log/slog` | stdlib | Structured logging, Go 1.21+ standard library |
| AWS SDK | `github.com/aws/aws-sdk-go-v2` | v2.x | Current generation, modular imports |
| Stripe | `github.com/stripe/stripe-go/v76` | v76.x | Official Stripe SDK |
| SendGrid | `github.com/sendgrid/sendgrid-go` | latest | Official SendGrid SDK |
| OAuth | `golang.org/x/oauth2` | latest | Standard library extension |
| UUID | `github.com/google/uuid` | latest | Client-side UUID generation when needed |
| Rate Limiting | `golang.org/x/time/rate` | latest | Token bucket rate limiter |

### 4.2 Python Dependencies (Eval Worker)

| Purpose | Package | Version |
|---------|---------|---------|
| Array processing | `numpy` | >=1.24 |
| Zarr storage | `zarr` | >=2.16 |
| S3 filesystem | `s3fs` | >=2023.6 |
| PostgreSQL | `psycopg2-binary` | >=2.9 |
| AWS SDK | `boto3` | >=1.28 |
| Data validation | `pydantic` | >=2.0 |

### 4.3 Infrastructure

| Component | Technology | Notes |
|-----------|------------|-------|
| Database | Supabase (Postgres 15) | Transaction pooler on port 6543 |
| Compute | AWS Lambda | Go 1.21+ (provided.al2023), Python 3.11 |
| Queues | AWS SQS | Standard queues with DLQ |
| Storage | AWS S3 | Zarr format for forecasts |
| API Gateway | AWS API Gateway HTTP | v2, Lambda proxy integration |
| Scheduling | AWS EventBridge | Cron and rate expressions |
| Secrets | AWS SSM Parameter Store | SecureString type |
| Monitoring | AWS CloudWatch | Metrics, logs, alarms |
| Payments | Stripe | Checkout, billing portal, webhooks |
| Email | SendGrid | Transactional email, templates |

### 4.4 Conventions

| Convention | Decision |
|------------|----------|
| Time storage | Always UTC in database |
| Time display | Convert to user timezone at API boundary |
| ID format | Prefixed UUIDs generated by DB (wp_, org_, user_, etc.) |
| JSON naming | snake_case for API fields |
| Go naming | Standard Go conventions (PascalCase exports, camelCase internal) |
| Python naming | PEP 8 (snake_case functions/variables, PascalCase classes) |
| Error responses | Consistent struct with code, message, details |

---

## 5. Sub-Document Inventory

### 5.1 Complete List

| # | Filename | Layer | Purpose | Scope |
|---|----------|-------|---------|-------|
| 01 | `01-foundation-types.md` | Foundation | Core domain types, interfaces, cross-cutting patterns | Large |
| 02 | `02-foundation-db.md` | Foundation | Database schema, migrations, repository interfaces | Large |
| 03 | `03-config.md` | Foundation | Configuration, environment variables, secrets | Medium |
| 04 | `04-sam-template.md` | Infrastructure | Complete SAM template specification | Large |
| 05a | `05a-api-core.md` | API | Router, middleware, error responses, health | Medium |
| 05b | `05b-api-watchpoints.md` | API | /v1/watchpoints/* handlers, bulk operations | Large |
| 05c | `05c-api-forecasts.md` | API | /v1/forecasts/* handlers | Medium |
| 05d | `05d-api-organization.md` | API | /v1/organization/*, /v1/users/*, /v1/api-keys/* | Medium |
| 05e | `05e-api-billing.md` | API | /v1/billing/*, /v1/usage/*, Stripe webhook | Medium |
| 05f | `05f-api-auth.md` | API | /auth/* handlers (login, OAuth, password reset) | Medium |
| 06 | `06-batcher.md` | Worker | Batcher Lambda, tile grouping, SQS enqueueing | Medium |
| 07 | `07-eval-worker.md` | Worker | Python eval worker, Zarr reading, condition evaluation | Large |
| 08a | `08a-notification-core.md` | Worker | Shared notification patterns, digest generation | Medium |
| 08b | `08b-email-worker.md` | Worker | Email delivery, SendGrid, templates | Small |
| 08c | `08c-webhook-worker.md` | Worker | Webhook delivery, platform formatting, HMAC | Medium |
| 09 | `09-scheduled-jobs.md` | Background | All EventBridge-triggered jobs | Medium |
| 10 | `10-external-integrations.md` | External | Stripe, SendGrid, OAuth provider integrations | Medium |
| 11 | `11-runpod.md` | External | RunPod inference interface and deployment | Small |
| 12 | `12-operations.md` | Operations | Local dev, deployment, monitoring, incidents | Medium |
| 13 | `13-human-setup.md` | Operations | AI-to-human guide for manual setup steps | Medium |
| 99 | `99-traceability-matrix.md` | Appendix | Complete flow-to-implementation mapping | Medium |

### 5.2 Scope Definitions

| Scope | Lines | Characteristics |
|-------|-------|-----------------|
| Small | 500-1000 | Focused scope, few dependencies |
| Medium | 1000-2000 | Moderate complexity |
| Large | 2000-3500 | Many types/functions, core to system |

**Total estimated**: ~30,000 lines across all sub-documents

---

## 6. Dependency Graph

### 6.1 Visual Representation

```
                         ┌─────────────────────────┐
                         │  01-foundation-types    │
                         │  (No dependencies)      │
                         └───────────┬─────────────┘
                                     │
            ┌────────────────────────┼────────────────────────┐
            ▼                        ▼                        ▼
┌───────────────────────┐ ┌───────────────────┐ ┌───────────────────────┐
│  02-foundation-db     │ │    03-config      │ │   04-sam-template     │
│  (Schema, repos)      │ │  (Env vars, etc.) │ │   (Infrastructure)    │
└───────────┬───────────┘ └─────────┬─────────┘ └───────────┬───────────┘
            │                       │                       │
            └───────────────────────┼───────────────────────┘
                                    │
                         ┌──────────┴──────────┐
                         ▼                     │
              ┌───────────────────┐            │
              │   05a-api-core    │            │
              │   (Router, MW)    │            │
              └─────────┬─────────┘            │
                        │                      │
   ┌──────────┬─────────┼─────────┬──────────┐│
   ▼          ▼         ▼         ▼          ▼▼
┌──────┐  ┌──────┐  ┌──────┐  ┌──────┐  ┌──────┐
│ 05b  │  │ 05c  │  │ 05d  │  │ 05e  │  │ 05f  │
│WP API│  │Fcst  │  │Org   │  │Bill  │  │Auth  │
└──────┘  └──────┘  └──────┘  └──────┘  └──────┘
                        │
   ┌────────────────────┼────────────────────┐
   ▼                    ▼                    ▼
┌──────────┐    ┌──────────────┐    ┌──────────────┐
│    06    │    │      07      │    │     08a      │
│ Batcher  │    │ Eval Worker  │    │ Notif Core   │
└──────────┘    └──────────────┘    └──────┬───────┘
                                           │
                                    ┌──────┴──────┐
                                    ▼             ▼
                              ┌──────────┐ ┌──────────┐
                              │   08b    │ │   08c    │
                              │  Email   │ │ Webhook  │
                              └──────────┘ └──────────┘
                                    │
   ┌────────────────────────────────┼─────────────────────────┐
   ▼                                ▼                         ▼
┌──────────────┐          ┌──────────────────┐       ┌──────────────┐
│      09      │          │        10        │       │      11      │
│  Sched Jobs  │          │ External Integr. │       │    RunPod    │
└──────────────┘          └──────────────────┘       └──────────────┘
                                    │
                         ┌──────────┴──────────┐
                         ▼                     ▼
              ┌───────────────────┐ ┌───────────────────┐
              │        12        │ │        13         │
              │   Operations     │ │   Human Setup     │
              └───────────────────┘ └───────────────────┘
                                    │
                                    ▼
                         ┌───────────────────┐
                         │        99         │
                         │ Traceability Mtx  │
                         └───────────────────┘
```

### 6.2 Dependency Rules

1. **No circular dependencies**: Arrows only point downward
2. **Foundation is root**: 01 depends on nothing
3. **SAM is parallel to foundation**: Needs config concepts but not implementations
4. **API docs share core**: All 05x docs depend on 05a
5. **Workers are independent**: 06, 07, 08x don't depend on each other
6. **Operations is terminal**: Depends on everything above

---

## 7. Build Order

### 7.1 Phased Build Sequence

| Phase | Documents | Dependencies Satisfied | Parallelizable |
|-------|-----------|------------------------|----------------|
| 1 | 01-foundation-types | None (root) | No |
| 2 | 02-foundation-db, 03-config | 01 complete | Yes |
| 3 | 04-sam-template | 01, 03 complete | No |
| 4 | 05a-api-core | 01, 02, 03 complete | No |
| 5 | 05b, 05c, 05d, 05e, 05f | 05a complete | Yes |
| 6 | 06-batcher | 01, 02, 03 complete | Yes |
| 7 | 07-eval-worker | 01, 02, 03 complete | Yes |
| 8 | 08a-notification-core | 01, 02 complete | Yes |
| 9 | 08b-email-worker, 08c-webhook-worker | 08a complete | Yes |
| 10 | 09-scheduled-jobs | 01, 02, 03 complete | Yes |
| 11 | 10-external-integrations | 01, 03 complete | Yes |
| 12 | 11-runpod | 01, 03 complete | Yes |
| 13 | 12-operations | All above complete | No |
| 14 | 13-human-setup | All above complete | No |
| 15 | 99-traceability-matrix | All above complete | No |

### 7.2 Serial Build Order (For Context-Limited Workflow)

Since we work file-by-file in separate chats:

```
01 → 02 → 03 → 04 → 05a → 05b → 05c → 05d → 05e → 05f →
06 → 07 → 08a → 08b → 08c → 09 → 10 → 11 → 12 → 13 → 99
```

Each document can reference all previously completed documents.

### 7.3 Estimated Effort

| Phase | Documents | Est. Sessions |
|-------|-----------|---------------|
| Foundation | 01, 02, 03 | 3 |
| Infrastructure | 04 | 1 |
| API | 05a-05f | 6 |
| Workers | 06, 07, 08a-08c | 5 |
| Background | 09 | 1 |
| External | 10, 11 | 2 |
| Operations | 12, 13 | 2 |
| Appendix | 99 | 1 |
| **Total** | **21 documents** | **~21 sessions** |

---

## 8. Sub-Document Specifications

### 8.1 Foundation Layer

#### 8.1.1 01-foundation-types.md

**Purpose**: Core domain types, interfaces, error handling, logging, validation, security, test mode, and audit patterns that all other documents reference.

**Dependencies**: None (root document)

**Dependents**: All other documents

**Must Contain**:

| Section | Contents |
|---------|----------|
| Domain Types | WatchPoint, Condition, Channel, Organization, User, APIKey, Session, Notification, ForecastRun |
| Enums | Status, ConditionOperator, ChannelType, ConditionLogic, ForecastType |
| Interfaces | NotificationChannel, Repository (base), ForecastSource, Evaluator |
| Error Types | AppError struct, error code constants, error constructors |
| Error Codes | Complete list from design addendum Section 3 |
| Logging | slog patterns, standard fields, log levels, correlation ID |
| Validation | Validators for conditions, webhooks, coordinates, time windows |
| Security | SSRF protection, brute force tracking, rate limiting interfaces |
| Test Mode | Detection function, behavior matrix, test_mode flag handling |
| Audit | AuditEvent struct, EmitAudit function, event types |
| Context | Request context patterns, correlation ID propagation |

**Flow Coverage**: Cross-cutting (enables all flows)

**Estimated Sections**:
1. Overview
2. Technology Decisions (Go dependencies)
3. Domain Types
4. Enums and Constants
5. Interfaces
6. Error Handling
7. Logging Conventions
8. Validation Patterns
9. Security Mechanisms
10. Test Mode
11. Audit Logging
12. Context Patterns

---

#### 8.1.2 02-foundation-db.md

**Purpose**: Database schema definition, migration strategy, connection patterns, and repository interfaces.

**Dependencies**: 01-foundation-types

**Dependents**: All API docs, all worker docs

**Must Contain**:

| Section | Contents |
|---------|----------|
| Connection | Supabase transaction pooler, pgx configuration, pool settings |
| Extensions | uuid-ossp, cube, earthdistance |
| Tables | Complete CREATE TABLE for all 12+ tables |
| Indexes | All indexes with performance rationale |
| Triggers | config_version increment trigger |
| Migrations | File naming, order, rollback strategy |
| Repository Pattern | Base interface, transaction handling |
| Repository Interfaces | Per-domain interfaces (WatchPoint, Organization, User, Notification, etc.) |

**Tables to Define**:
- organizations
- users
- watchpoints
- watchpoint_evaluation_state
- notifications
- notification_deliveries
- api_keys
- sessions
- audit_log
- login_attempts
- forecast_runs
- calibration_coefficients

**Flow Coverage**: Data operations for all flows

**Estimated Sections**:
1. Overview
2. Connection Management
3. Required Extensions
4. Schema Definition (all tables)
5. Indexes and Performance
6. Triggers
7. Migration Strategy
8. Migration File Inventory
9. Repository Pattern
10. Repository Interfaces
11. Transaction Patterns
12. Query Patterns

---

#### 8.1.3 03-config.md

**Purpose**: Configuration structure, environment variables, secrets management, and SSM parameter patterns.

**Dependencies**: 01-foundation-types

**Dependents**: All docs that use configuration

**Must Contain**:

| Section | Contents |
|---------|----------|
| Config Struct | Complete Go struct with all configuration fields |
| Environment Variables | Complete list with names, types, defaults |
| Secrets Inventory | All secrets, their SSM paths, who provides them |
| Loading Pattern | How config is loaded at Lambda cold start |
| Validation | Config validation at startup |
| Per-Environment | dev/staging/prod differences |

**Secrets to Document**:
- DATABASE_URL
- STRIPE_SECRET_KEY
- STRIPE_WEBHOOK_SECRET
- SENDGRID_API_KEY
- RUNPOD_API_KEY
- OAuth client secrets (Google, GitHub)

**Flow Coverage**: Enables all flows (configuration is universal)

**Estimated Sections**:
1. Overview
2. Configuration Struct
3. Environment Variables
4. Secrets Inventory
5. SSM Parameter Patterns
6. Loading and Validation
7. Per-Environment Configuration

---

### 8.2 Infrastructure Layer

#### 8.2.1 04-sam-template.md

**Purpose**: Complete AWS SAM template specification defining all Lambda functions, queues, buckets, API Gateway, EventBridge rules, IAM roles, and CloudWatch alarms.

**Dependencies**: 01-foundation-types, 03-config

**Dependents**: All Lambda-specific docs reference this for deployment config

**Must Contain**:

| Section | Contents |
|---------|----------|
| Globals | Shared Lambda configuration |
| Parameters | Environment, domain name, etc. |
| Lambda Functions | All 8 functions with full configuration |
| API Gateway | HTTP API with routes |
| SQS Queues | eval-urgent, eval-standard, notification, DLQs |
| S3 Buckets | Forecast storage bucket |
| EventBridge Rules | All scheduled triggers |
| IAM Roles | Per-function roles with least privilege |
| CloudWatch Alarms | Dead man's switch, queue depth, errors |
| Outputs | API URL, resource ARNs |

**Lambda Functions to Define**:
- api (Go)
- batcher (Go)
- eval-worker-urgent (Python)
- eval-worker-standard (Python)
- email-worker (Go)
- webhook-worker (Go)
- data-poller (Go)
- archiver (Go)

**Flow Coverage**: Deployment infrastructure for all flows

**Estimated Sections**:
1. Overview
2. Template Parameters
3. Global Configuration
4. Lambda Functions
5. API Gateway
6. SQS Queues
7. S3 Buckets
8. EventBridge Rules
9. IAM Roles and Policies
10. CloudWatch Alarms
11. Outputs
12. Complete Template

---

### 8.3 API Layer

#### 8.3.1 05a-api-core.md

**Purpose**: Router setup, middleware stack, error response formatting, health endpoint, and common API patterns.

**Dependencies**: 01-foundation-types, 02-foundation-db, 03-config

**Dependents**: All 05x API docs

**Must Contain**:

| Section | Contents |
|---------|----------|
| Router | chi router setup, route registration pattern |
| Middleware Stack | Order and configuration of all middleware |
| Authentication MW | API key and session validation |
| Authorization MW | Permission checking pattern |
| Rate Limiting MW | Rate limit checking per endpoint |
| Request Logging | Structured request/response logging |
| Error Responses | Standard error response format |
| Health Endpoint | GET /v1/health implementation |
| Panic Recovery | Recovery middleware |
| CORS | CORS configuration |
| Request Context | How context flows through handlers |

**Middleware Order**:
1. Panic recovery
2. Request ID / Correlation ID
3. Request logging
4. CORS
5. Rate limiting
6. Authentication
7. Authorization

**Flow Coverage**: API-001 (Request lifecycle), API-003 (Health check)

**Estimated Sections**:
1. Overview
2. Router Setup
3. Middleware Stack
4. Authentication Middleware
5. Authorization Middleware
6. Rate Limiting Middleware
7. Error Response Format
8. Health Endpoint
9. Request Context
10. Panic Recovery
11. CORS Configuration
12. Vertical App Extension Points

---

#### 8.3.2 05b-api-watchpoints.md

**Purpose**: All /v1/watchpoints/* handlers including CRUD, pause/resume, clone, and bulk operations.

**Dependencies**: 05a-api-core

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Handler Struct | WatchPointHandler with dependencies |
| Request Types | All request structs with validation tags |
| Response Types | All response structs with JSON tags |
| Endpoints | Full specification for each endpoint |
| Bulk Operations | Bulk create, pause, resume, delete, tags, clone |
| Validation | Domain-specific validation rules |
| Error Mapping | Which errors for which conditions |

**Endpoints to Define**:
- POST /v1/watchpoints (create)
- GET /v1/watchpoints (list)
- GET /v1/watchpoints/{id} (get)
- PATCH /v1/watchpoints/{id} (update)
- DELETE /v1/watchpoints/{id} (delete)
- POST /v1/watchpoints/{id}/pause
- POST /v1/watchpoints/{id}/resume
- POST /v1/watchpoints/{id}/clone
- GET /v1/watchpoints/{id}/notifications
- POST /v1/watchpoints/bulk
- POST /v1/watchpoints/bulk/pause
- POST /v1/watchpoints/bulk/resume
- POST /v1/watchpoints/bulk/delete
- PATCH /v1/watchpoints/bulk/tags
- POST /v1/watchpoints/bulk/clone

**Flow Coverage**: WPLC-001 through WPLC-012, BULK-001 through BULK-006, INFO-008

**Estimated Sections**:
1. Overview
2. Handler Structure
3. Request Types
4. Response Types
5. Create Endpoint (Event & Monitor)
6. Read Endpoints (Get, List)
7. Update Endpoint
8. Delete Endpoint
9. Pause/Resume Endpoints
10. Clone Endpoint
11. Notification History Endpoint
12. Bulk Operations
13. Validation Rules
14. Error Handling
15. Flow Coverage

---

#### 8.3.3 05c-api-forecasts.md

**Purpose**: Forecast query endpoints for point queries, batch queries, variable listing, and status.

**Dependencies**: 05a-api-core

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Handler Struct | ForecastHandler with dependencies |
| Request Types | Point query, batch query request structs |
| Response Types | Forecast data response structs |
| Zarr Access | How handlers read from Zarr (via service) |
| Caching | Response caching strategy |
| Variable Mapping | Available variables and their metadata |

**Endpoints to Define**:
- GET /v1/forecasts/point (single location forecast)
- POST /v1/forecasts/points (batch location forecast)
- GET /v1/forecasts/variables (list available variables)
- GET /v1/forecasts/status (forecast freshness status)

**Flow Coverage**: FQRY-001 through FQRY-004

**Estimated Sections**:
1. Overview
2. Handler Structure
3. Request Types
4. Response Types
5. Point Query Endpoint
6. Batch Query Endpoint
7. Variables Endpoint
8. Status Endpoint
9. Forecast Service Interface
10. Caching Strategy
11. Flow Coverage

---

#### 8.3.4 05d-api-organization.md

**Purpose**: Organization, user, and API key management endpoints.

**Dependencies**: 05a-api-core

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Handlers | OrganizationHandler, UserHandler, APIKeyHandler |
| Request/Response Types | All types for each endpoint |
| Role Management | Role definitions, permission checks |
| Invite Flow | User invitation and acceptance |
| API Key Management | Create, rotate, revoke |

**Endpoints to Define**:
- GET /v1/organization
- PATCH /v1/organization
- GET /v1/organization/notification-preferences
- PATCH /v1/organization/notification-preferences
- GET /v1/users
- POST /v1/users/invite
- PATCH /v1/users/{id}
- DELETE /v1/users/{id}
- GET /v1/api-keys
- POST /v1/api-keys
- DELETE /v1/api-keys/{id}
- POST /v1/api-keys/{id}/rotate

**Flow Coverage**: USER-002, USER-004, USER-005, USER-009 through USER-013

**Estimated Sections**:
1. Overview
2. Organization Handler
3. User Handler
4. API Key Handler
5. Request Types
6. Response Types
7. Role Definitions
8. Invite Flow
9. API Key Lifecycle
10. Flow Coverage

---

#### 8.3.5 05e-api-billing.md

**Purpose**: Billing, usage, and Stripe integration endpoints.

**Dependencies**: 05a-api-core

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Handlers | BillingHandler, UsageHandler, StripeWebhookHandler |
| Stripe Integration | Checkout session, portal session creation |
| Usage Tracking | Current usage, usage history |
| Webhook Handling | Stripe event processing |
| Plan Enforcement | How limits are checked and enforced |

**Endpoints to Define**:
- GET /v1/usage
- GET /v1/usage/history
- GET /v1/billing/invoices
- GET /v1/billing/subscription
- POST /v1/billing/checkout-session
- POST /v1/billing/portal-session
- POST /v1/webhooks/stripe

**Flow Coverage**: BILL-001 through BILL-007, INFO-003 through INFO-006

**Estimated Sections**:
1. Overview
2. Billing Handler
3. Usage Handler
4. Stripe Webhook Handler
5. Request Types
6. Response Types
7. Checkout Session Flow
8. Portal Session Flow
9. Webhook Event Handling
10. Plan Enforcement
11. Flow Coverage

---

#### 8.3.6 05f-api-auth.md

**Purpose**: Authentication endpoints including login, logout, OAuth, and password management.

**Dependencies**: 05a-api-core

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Handlers | AuthHandler |
| Session Management | Create, validate, invalidate sessions |
| OAuth Flow | Google and GitHub OAuth handling |
| Password Flow | Login, forgot password, reset password |
| CSRF Protection | Token generation and validation |
| Brute Force | Login attempt tracking |

**Endpoints to Define**:
- POST /auth/login
- POST /auth/logout
- POST /auth/forgot-password
- POST /auth/reset-password
- GET /auth/oauth/{provider}/callback
- POST /auth/accept-invite

**Flow Coverage**: DASH-001 through DASH-004, USER-005, USER-008

**Estimated Sections**:
1. Overview
2. Auth Handler
3. Request Types
4. Response Types
5. Login Flow
6. Logout Flow
7. OAuth Flow
8. Password Reset Flow
9. Invite Acceptance Flow
10. Session Management
11. CSRF Protection
12. Brute Force Protection
13. Flow Coverage

---

### 8.4 Worker Layer

#### 8.4.1 06-batcher.md

**Purpose**: Batcher Lambda that responds to S3 forecast events, groups WatchPoints by tile, and enqueues evaluation jobs.

**Dependencies**: 01-foundation-types, 02-foundation-db, 03-config

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Lambda Handler | S3 event handler signature |
| Tile Grouping | GROUP BY query, index-only scan |
| Hot Tile Pagination | Splitting tiles >500 WatchPoints |
| Queue Selection | Urgent vs standard queue routing |
| SQS Message Format | Message schema (THE CONTRACT with eval worker) |
| Metrics | ForecastReady metric for dead man's switch |
| Error Handling | Partial failure handling |

**SQS Message Schema** (Critical Contract):
```json
{
  "forecast_type": "medium_range" | "nowcast",
  "forecast_timestamp": "2026-01-31T06:00:00Z",
  "tile_id": "3.2",
  "s3_path": "s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z/",
  "page": 0,
  "page_size": 500,
  "total_watchpoints": 1247
}
```

**Flow Coverage**: EVAL-001 (Batcher phase), EVAL-002 (Hot tile pagination)

**Estimated Sections**:
1. Overview
2. Lambda Handler
3. S3 Event Processing
4. Tile Grouping Query
5. Hot Tile Pagination
6. Queue Selection Logic
7. SQS Message Format
8. Metrics Emission
9. Error Handling
10. Flow Coverage

---

#### 8.4.2 07-eval-worker.md

**Purpose**: Python evaluation worker that consumes SQS messages, reads Zarr forecasts, evaluates conditions, and queues notifications.

**Dependencies**: 01-foundation-types, 02-foundation-db, 03-config

**Dependents**: None

**Language**: Python (not Go)

**Must Contain**:

| Section | Contents |
|---------|----------|
| Module Structure | handler.py, forecast_reader.py, evaluation.py |
| Type Definitions | Python dataclasses/TypedDict with type hints |
| SQS Consumption | Message parsing, acknowledgment |
| Zarr Reading | S3 access, tile extraction, halo regions |
| Condition Evaluation | Operator implementations, threshold comparison |
| State Transitions | Trigger state changes, hysteresis |
| Notification Queuing | When and what to queue |
| Monitor Mode | Threat hashing, deduplication |
| Config Version | Race condition detection |

**Python Function Signatures**:
```python
def handler(event: dict, context: Any) -> dict:
    """Lambda entrypoint."""

def evaluate_tile(message: TileMessage) -> EvaluationResult:
    """Evaluate all WatchPoints in a tile."""

def read_forecast(s3_path: str, tile_id: str) -> xr.Dataset:
    """Read Zarr forecast data for a tile."""

def evaluate_conditions(wp: WatchPoint, forecast: xr.Dataset) -> bool:
    """Evaluate WatchPoint conditions against forecast."""
```

**Flow Coverage**: EVAL-001 (Evaluation phase), EVAL-003 (Monitor mode), EVAL-004 (Config mismatch), EVAL-005 (First evaluation)

**Estimated Sections**:
1. Overview
2. Module Structure
3. Type Definitions
4. Lambda Handler
5. SQS Message Processing
6. Forecast Reading
7. Condition Evaluation
8. State Transition Logic
9. Notification Queuing
10. Monitor Mode Handling
11. Config Version Checking
12. Error Handling
13. Flow Coverage

---

#### 8.4.3 08a-notification-core.md

**Purpose**: Shared notification patterns, delivery tracking, retry logic, quiet hours, test mode, and digest generation.

**Dependencies**: 01-foundation-types, 02-foundation-db

**Dependents**: 08b-email-worker, 08c-webhook-worker

**Must Contain**:

| Section | Contents |
|---------|----------|
| Data Model | Notification, NotificationDelivery structs |
| SQS Message Format | Message from eval worker |
| Delivery Tracking | Status transitions, attempt logging |
| Retry Logic | Exponential backoff, max attempts |
| Quiet Hours | Check logic, suppression behavior |
| Test Mode | Detection, logging without delivery |
| Channel Interface | NotificationChannel interface |
| Digest Generation | DigestGenerator, DigestContent struct |
| Escalation | Detecting and handling escalations |
| Cleared Notifications | Handling condition clearance |

**NotificationChannel Interface**:
```go
type NotificationChannel interface {
    Type() ChannelType
    Format(notification *Notification) ([]byte, error)
    Deliver(ctx context.Context, payload []byte, destination string) (*DeliveryResult, error)
    ShouldRetry(err error) bool
}
```

**Flow Coverage**: NOTIF-001 (core), NOTIF-002 (quiet hours), NOTIF-003 (digest), NOTIF-008 (escalation), NOTIF-009 (cleared)

**Estimated Sections**:
1. Overview
2. Notification Data Model
3. SQS Message Format
4. Delivery Tracking
5. Retry Logic
6. Quiet Hours Handling
7. Test Mode Handling
8. NotificationChannel Interface
9. Digest Generation
10. Escalation Handling
11. Cleared Notifications
12. Flow Coverage

---

#### 8.4.4 08b-email-worker.md

**Purpose**: Email notification delivery using SendGrid, including templates and bounce handling.

**Dependencies**: 08a-notification-core

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Worker Structure | EmailWorker with dependencies |
| SendGrid Client | Client wrapper, API calls |
| Email Formatting | Template rendering |
| Templates | Template IDs and variables |
| Bounce Handling | Webhook processing for bounces |
| Error Mapping | SendGrid errors to retry decisions |

**Templates**:
- watchpoint-triggered
- watchpoint-cleared
- watchpoint-escalated
- digest
- invite
- password-reset

**Flow Coverage**: NOTIF-001 (email delivery), NOTIF-006 (bounce handling)

**Estimated Sections**:
1. Overview
2. Worker Structure
3. SendGrid Client
4. Email Formatting
5. Template Definitions
6. Delivery Logic
7. Bounce Handling
8. Error Handling
9. Flow Coverage

---

#### 8.4.5 08c-webhook-worker.md

**Purpose**: Webhook notification delivery with platform detection, HMAC signing, and SSRF protection.

**Dependencies**: 08a-notification-core

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Worker Structure | WebhookWorker with dependencies |
| Platform Detection | Detecting Slack, Discord, Teams, Generic |
| Formatters | Platform-specific payload formatting |
| HMAC Signing | Signature generation for verification |
| SSRF Protection | URL validation before delivery |
| Event Sequence | Ordering via event_sequence field |
| Retry-After | Respecting rate limit headers |

**Platform Formatters**:
- Slack (Block Kit)
- Discord (Embeds)
- Microsoft Teams (Adaptive Cards)
- Google Chat (Cards)
- Generic (Standard JSON)

**Flow Coverage**: NOTIF-001 (webhook delivery), NOTIF-004 (retry), NOTIF-005 (rate limiting), NOTIF-007 (platform formatting)

**Estimated Sections**:
1. Overview
2. Worker Structure
3. Platform Detection
4. Formatter Registry
5. Platform Formatters
6. HMAC Signature Generation
7. SSRF Protection
8. Event Sequence Handling
9. Rate Limit Handling
10. Delivery Logic
11. Error Handling
12. Flow Coverage

---

### 8.5 Background Jobs Layer

#### 8.5.1 09-scheduled-jobs.md

**Purpose**: All EventBridge-triggered scheduled jobs including data poller, archiver, digest scheduler, and maintenance jobs.

**Dependencies**: 01-foundation-types, 02-foundation-db, 03-config

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Job Inventory | All jobs with schedules |
| Common Patterns | Job handler signature, idempotency, metrics |
| Data Poller | Checking for new forecast data |
| Archiver | Archiving expired WatchPoints |
| Digest Scheduler | Triggering per-org digest generation |
| Cleanup Jobs | Various maintenance cleanup |
| Verification | Forecast verification pipeline |
| Usage Aggregation | Daily usage stats |
| Stripe Sync | Subscription status sync |

**Jobs to Define**:
- Data Poller (every 15 min)
- Archiver (every 15 min)
- Digest Scheduler (every 1 min)
- Rate Limit Reset (daily)
- Usage Aggregation (daily)
- Stripe Sync (daily)
- Forecast Tier Transition (daily)
- Archived WatchPoint Cleanup (daily)
- Soft-Deleted Org Cleanup (daily)
- Expired Invite Cleanup (daily)
- Verification Pipeline (daily)

**Flow Coverage**: FCST-003, WPLC-009, SCHED-001 through SCHED-004, MAINT-001 through MAINT-006, OBS-005

**Estimated Sections**:
1. Overview
2. Job Inventory
3. Common Patterns
4. Data Jobs (Poller, Tier Transition)
5. WatchPoint Maintenance (Archiver, Cleanup)
6. Organization Maintenance (Soft-delete, Invites)
7. Billing Jobs (Usage, Stripe Sync)
8. Observability Jobs (Verification)
9. Digest Scheduler
10. Error Handling
11. Flow Coverage

---

### 8.6 External Systems Layer

#### 8.6.1 10-external-integrations.md

**Purpose**: Third-party service integrations including Stripe, SendGrid, and OAuth providers.

**Dependencies**: 01-foundation-types, 03-config

**Dependents**: 05e-api-billing, 05f-api-auth, 08b-email-worker

**Must Contain**:

| Section | Contents |
|---------|----------|
| Common Patterns | API client structure, retry policy, circuit breaker |
| Stripe | Outbound API calls, incoming webhooks |
| SendGrid | Outbound API calls, incoming webhooks |
| Google OAuth | Authorization URL, token exchange, user info |
| GitHub OAuth | Authorization URL, token exchange, user info |
| Webhook Security | Signature verification patterns |

**Flow Coverage**: BILL-001 through BILL-007 (Stripe), NOTIF-006 (SendGrid), DASH-002 (OAuth)

**Estimated Sections**:
1. Overview
2. Common Patterns
3. Stripe Integration
4. SendGrid Integration
5. Google OAuth
6. GitHub OAuth
7. Webhook Security
8. Error Handling
9. Flow Coverage

---

#### 8.6.2 11-runpod.md

**Purpose**: RunPod inference interface, trigger API, Zarr output contract, and deployment.

**Dependencies**: 01-foundation-types, 03-config

**Dependents**: 09-scheduled-jobs (data poller)

**Must Contain**:

| Section | Contents |
|---------|----------|
| Interface Contract | Trigger API request/response |
| Output Contract | Zarr structure, _SUCCESS marker |
| Zarr Schema | Dimensions, variables, chunking |
| Dockerfile | Container structure |
| Deployment | RunPod serverless endpoint setup |
| Error Handling | Retry behavior, failure modes |

**NOT Included**:
- Model architecture details
- Training process
- Hyperparameters

**Flow Coverage**: FCST-001 (medium-range), FCST-002 (nowcast), FCST-004 (retry), FCST-005 (fallback)

**Estimated Sections**:
1. Overview
2. Interface Contract
3. Trigger API
4. Output Contract
5. Zarr Schema
6. Container Structure
7. Deployment Process
8. Error Handling
9. Monitoring
10. Flow Coverage

---

### 8.7 Operations Layer

#### 8.7.1 12-operations.md

**Purpose**: Local development setup, deployment procedures, monitoring configuration, and incident response.

**Dependencies**: All previous documents

**Dependents**: 13-human-setup

**Must Contain**:

| Section | Contents |
|---------|----------|
| Local Development | docker-compose, scripts, sample data |
| Deployment | SAM deployment, environments |
| Monitoring | CloudWatch dashboards, key metrics |
| Alerting | Alarm configuration, escalation |
| Incident Response | Runbooks for common issues |
| Backup | Database backup strategy |
| Disaster Recovery | Recovery procedures |

**Flow Coverage**: OBS-001 through OBS-004 (monitoring), FAIL-001 through FAIL-006 (recovery)

**Estimated Sections**:
1. Overview
2. Local Development Setup
3. Deployment Procedures
4. Environment Configuration
5. Monitoring Dashboards
6. Alerting Configuration
7. Incident Response Runbooks
8. Backup Strategy
9. Disaster Recovery
10. Flow Coverage

---

#### 8.7.2 13-human-setup.md

**Purpose**: Guide for AI agents on what to ask humans to do, including account creation, secrets provision, and manual configuration.

**Dependencies**: All previous documents

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Setup Order | What order to do manual steps |
| AWS Account | Account creation, billing alerts |
| Supabase | Project creation, connection string |
| Stripe | Account, webhook configuration |
| SendGrid | Account, API key, domain verification |
| RunPod | Account, endpoint deployment |
| OAuth Apps | Google, GitHub app registration |
| Domain | Registration, DNS configuration |
| SSM Population | How to populate all secrets |
| AI Prompts | Exact prompts for AI to use when asking human |

**Format**: Written FOR the AI agent, telling it what to prompt the human

**Flow Coverage**: Enables all flows (setup is prerequisite)

**Estimated Sections**:
1. Overview
2. Setup Order
3. AWS Account Setup
4. Supabase Setup
5. Stripe Setup
6. SendGrid Setup
7. RunPod Setup
8. OAuth App Registration
9. Domain and DNS
10. SSM Parameter Population
11. Verification Steps
12. AI-to-Human Prompts

---

### 8.8 Appendices

#### 8.8.1 99-traceability-matrix.md

**Purpose**: Complete mapping of all ~160 flows to their implementing components.

**Dependencies**: All previous documents

**Dependents**: None

**Must Contain**:

| Section | Contents |
|---------|----------|
| Flow Matrix | Every flow ID mapped to sub-document and function |
| Endpoint Matrix | Every API endpoint mapped to handler |
| Table Matrix | Every table mapped to accessing components |
| Queue Matrix | Every queue mapped to producers/consumers |
| Validation Checklist | Checklist for verifying completeness |

**Format**: Primarily tables for easy lookup and validation

**Flow Coverage**: Meta-document covering all flows

**Estimated Sections**:
1. Overview
2. Complete Flow Traceability Matrix
3. API Endpoint Mapping
4. Database Table Mapping
5. SQS Queue Mapping
6. Validation Checklist
7. Gap Analysis Template

---

## 9. Cross-Cutting Concerns

### 9.1 Where Each Concern Is Defined

| Concern | Primary Definition | Applied In |
|---------|-------------------|------------|
| Error Handling | 01-foundation-types §6 | All API and worker docs |
| Logging | 01-foundation-types §7 | All docs |
| Validation | 01-foundation-types §8 | 05a, 05b, all API docs |
| Security (SSRF) | 01-foundation-types §9 | 08c-webhook-worker |
| Security (Brute Force) | 01-foundation-types §9 | 05f-api-auth |
| Security (Rate Limiting) | 01-foundation-types §9 | 05a-api-core |
| Test Mode | 01-foundation-types §10 | All API docs, 08a notification |
| Audit Logging | 01-foundation-types §11 | All mutating API handlers |
| Authentication | 01-foundation-types §5, 05a §4 | All authenticated endpoints |
| Authorization | 01-foundation-types §5, 05a §5 | All authorized endpoints |
| Configuration | 03-config | All Lambda functions |
| Database Access | 02-foundation-db | All data-accessing components |

### 9.2 Application Pattern

Each sub-document that uses a cross-cutting concern:
1. References the foundation section where it's defined
2. States which specific aspect applies (e.g., "Uses API key authentication")
3. Notes any context-specific variations

Example in 05b-api-watchpoints.md:
```markdown
## 3. Authentication & Authorization

Uses API key authentication (see 01-foundation-types §5).

| Endpoint | Auth Required | Permission |
|----------|---------------|------------|
| POST /v1/watchpoints | Yes | watchpoints:write |
| GET /v1/watchpoints | Yes | watchpoints:read |
```

---

## 10. Flow Coverage Summary

### 10.1 By Domain

| Domain | Flow IDs | Count | Primary Sub-Documents |
|--------|----------|-------|----------------------|
| FCST | FCST-001 to FCST-006 | 6 | 09, 11 |
| EVAL | EVAL-001 to EVAL-006 | 6 | 06, 07 |
| NOTIF | NOTIF-001 to NOTIF-010 | 10 | 08a, 08b, 08c |
| WPLC | WPLC-001 to WPLC-012 | 12 | 05b |
| USER | USER-001 to USER-013 | 13 | 05d, 05f |
| BILL | BILL-001 to BILL-007 | 7 | 05e, 10 |
| OBS | OBS-001 to OBS-011 | 11 | 09, 12 |
| MAINT | MAINT-001 to MAINT-006 | 6 | 09 |
| SCHED | SCHED-001 to SCHED-004 | 4 | 09 |
| API | API-001 to API-003 | 3 | 05a |
| FQRY | FQRY-001 to FQRY-004 | 4 | 05c |
| INFO | INFO-001 to INFO-008 | 8 | 05b, 05c, 05d, 05e |
| DASH | DASH-001 to DASH-006 | 6 | 05e, 05f |
| HOOK | HOOK-001 to HOOK-003 | 3 | 08c |
| BULK | BULK-001 to BULK-006 | 6 | 05b |
| VALID | VALID-001 to VALID-003 | 3 | 01 (foundation) |
| SEC | SEC-001 to SEC-004 | 4 | 01 (foundation) |
| TEST | TEST-001 to TEST-003 | 3 | 01 (foundation) |
| VERT | VERT-001 to VERT-002 | 2 | 05a (extension points) |
| FAIL | FAIL-001 to FAIL-006 | 6 | Distributed |
| CONC | CONC-001 to CONC-006 | 6 | Distributed |
| IMPL | IMPL-001 to IMPL-004 | 4 | Distributed |
| INT | INT-001 to INT-004 | 4 | Cross-cutting |
| **TOTAL** | | **~160** | |

### 10.2 Validation Approach

After all sub-documents are complete:
1. Extract all Flow IDs from `watchpoint-flows-v1.md`
2. Search each sub-document for flow coverage tables
3. Verify every Flow ID appears at least once
4. Flag any gaps for resolution
5. Populate `99-traceability-matrix.md` with complete mapping

---

## 11. Completeness Checklist

### 11.1 Flow Coverage

- [ ] All ~160 flows from watchpoint-flows-v1.md have assigned sub-document
- [ ] No flow is marked "TBD" or "unassigned"
- [ ] Multi-component flows have all components identified
- [ ] Flow coverage tables exist in each sub-document

### 11.2 Data Layer Coverage

- [ ] All database tables have schema in 02-foundation-db
  - [ ] organizations
  - [ ] users
  - [ ] watchpoints
  - [ ] watchpoint_evaluation_state
  - [ ] notifications
  - [ ] notification_deliveries
  - [ ] api_keys
  - [ ] sessions
  - [ ] audit_log
  - [ ] login_attempts
  - [ ] forecast_runs
  - [ ] calibration_coefficients
- [ ] All indexes documented with rationale
- [ ] All triggers documented
- [ ] Repository interfaces defined for each table grouping

### 11.3 Compute Layer Coverage

- [ ] All Lambda functions have sub-document assignment
  - [ ] api → 05a + 05b-05f
  - [ ] batcher → 06
  - [ ] eval-worker-urgent → 07
  - [ ] eval-worker-standard → 07
  - [ ] email-worker → 08b
  - [ ] webhook-worker → 08c
  - [ ] data-poller → 09
  - [ ] archiver → 09
- [ ] RunPod inference → 11

### 11.4 API Coverage

- [ ] All endpoints from design addendum have handler assignments
- [ ] Request/response types defined for each endpoint
- [ ] Error codes mapped to each endpoint
- [ ] Authentication requirements specified per endpoint

### 11.5 Infrastructure Coverage

- [ ] SAM template resources enumerated in 04
  - [ ] Lambda functions
  - [ ] SQS queues
  - [ ] S3 buckets
  - [ ] API Gateway
  - [ ] EventBridge rules
  - [ ] CloudWatch alarms
  - [ ] IAM roles
- [ ] Environment variables listed
- [ ] SSM parameters listed

### 11.6 External Integration Coverage

- [ ] All external services documented in 10
  - [ ] Stripe (API + webhooks)
  - [ ] SendGrid (API + webhooks)
  - [ ] Google OAuth
  - [ ] GitHub OAuth
- [ ] NOAA data sources documented
- [ ] RunPod API documented in 11

### 11.7 Cross-Cutting Concerns

- [ ] Error handling patterns in 01
- [ ] Logging conventions in 01
- [ ] Authentication patterns in 01
- [ ] Authorization patterns in 01
- [ ] Validation patterns in 01
- [ ] Security mechanisms in 01
- [ ] Test mode behavior in 01
- [ ] Audit logging patterns in 01

### 11.8 Dependency Validation

- [ ] Dependency graph has no cycles
- [ ] Build order is valid
- [ ] All cross-references are bidirectional

### 11.9 Human Setup Coverage

- [ ] All manual setup steps captured in 13
  - [ ] AWS account
  - [ ] Supabase project
  - [ ] Stripe account
  - [ ] SendGrid account
  - [ ] RunPod account
  - [ ] OAuth apps
  - [ ] Domain/DNS
  - [ ] SSM parameters
- [ ] Order of operations specified
- [ ] AI-to-human prompts written

### 11.10 Operations Coverage

- [ ] Local development setup in 12
- [ ] Deployment procedure in 12
- [ ] Monitoring and alerting in 12
- [ ] Incident response in 12
- [ ] Backup and recovery in 12

---

## 12. Document Conventions

### 12.1 File Naming

```
architecture/
├── 00-architecture-plan.md          ← This document
├── 01-foundation-types.md
├── 02-foundation-db.md
├── 03-config.md
├── 04-sam-template.md
├── 05a-api-core.md
├── 05b-api-watchpoints.md
├── 05c-api-forecasts.md
├── 05d-api-organization.md
├── 05e-api-billing.md
├── 05f-api-auth.md
├── 06-batcher.md
├── 07-eval-worker.md
├── 08a-notification-core.md
├── 08b-email-worker.md
├── 08c-webhook-worker.md
├── 09-scheduled-jobs.md
├── 10-external-integrations.md
├── 11-runpod.md
├── 12-operations.md
├── 13-human-setup.md
└── 99-traceability-matrix.md
```

**Rules**:
- Two-digit prefix ensures sort order
- Lowercase with hyphens
- Letters for sub-documents (05a, 05b)
- `.md` extension

### 12.2 Section Structure Template

Each sub-document follows this structure:

```markdown
# {Document Title}

> One-line description of this document's purpose.

**Dependencies**: List of required docs
**Dependents**: List of docs that require this
**Flows Covered**: Flow ID ranges

---

## Table of Contents

1. [Overview](#1-overview)
2. [Dependencies](#2-dependencies)
3. [{Domain-Specific Sections}]
...
N-1. [Flow Coverage](#n-1-flow-coverage)
N. [Document History](#n-document-history)

---

## 1. Overview

Brief description of component's role.

## 2. Dependencies

### 2.1 Imports
### 2.2 Imported By

## 3-N. {Content Sections}

## N-1. Flow Coverage

| Flow ID | Flow Name | Implementation |
|---------|-----------|----------------|

## N. Document History

| Version | Date | Changes |
|---------|------|---------|
```

### 12.3 Code Block Conventions

```markdown
Go code (actual signatures):
```go
func (h *Handler) Create(w http.ResponseWriter, r *http.Request)
```

Python code (actual signatures):
```python
def evaluate_tile(message: TileMessage) -> EvaluationResult:
```

SQL (schema definitions):
```sql
CREATE TABLE watchpoints ( ... );
```

YAML (SAM/config):
```yaml
Resources:
  ApiFunction:
    Type: AWS::Serverless::Function
```

JSON (API payloads, SQS messages):
```json
{"forecast_type": "nowcast", "tile_id": "3.2"}
```

Pseudocode (logic flows):
```
FOR each watchpoint in tile:
    IF conditions_met(watchpoint, forecast):
        queue_notification(watchpoint)
```
```

### 12.4 Cross-Reference Format

```markdown
Within same doc:
See [Section 3.2](#32-repository-interfaces)

To another doc:
See `02-foundation-db.md` Section 4

To flows doc:
Implements flow `WPLC-001` (see watchpoint-flows-v1.md)
```

### 12.5 Version Tracking

- Each sub-document has Document History section
- Combined document gets single version
- Source documents remain at individual versions

---

## 13. Final Assembly Process

### 13.1 When to Assemble

After all 21 sub-documents are complete and validated.

### 13.2 Assembly Steps

1. **Concatenation**: Combine in numerical order
2. **Redundancy Removal**:
   - Remove individual doc headers/footers
   - Consolidate repeated type definitions
   - Merge Document History sections
3. **Synthesis Additions**:
   - System Overview section with architecture diagram
   - Master Table of Contents
   - Quick Reference appendices (all env vars, all endpoints, all tables)
   - Combined Glossary
4. **Table of Contents Generation**: Auto-generate from headers
5. **Validation**: Run against completeness checklist

### 13.3 Combined Document Structure

```markdown
# WatchPoint Platform Architecture

## Part I: Foundations
- Core Types & Interfaces (from 01)
- Database Schema & Repositories (from 02)
- Configuration (from 03)

## Part II: Infrastructure
- SAM Template & AWS Resources (from 04)

## Part III: API Layer
- API Core (from 05a)
- WatchPoint Endpoints (from 05b)
- Forecast Endpoints (from 05c)
- Organization Endpoints (from 05d)
- Billing Endpoints (from 05e)
- Authentication Endpoints (from 05f)

## Part IV: Workers
- Batcher (from 06)
- Evaluation Worker (from 07)
- Notification Core (from 08a)
- Email Worker (from 08b)
- Webhook Worker (from 08c)

## Part V: Background Jobs
- Scheduled Jobs (from 09)

## Part VI: External Systems
- External Integrations (from 10)
- RunPod Inference (from 11)

## Part VII: Operations
- Deployment & Operations (from 12)
- Human Setup Guide (from 13)

## Appendices
- A. System Architecture Diagram
- B. Complete Flow Traceability Matrix (from 99)
- C. API Endpoint Reference
- D. Database Schema Quick Reference
- E. Environment Variables Reference
- F. Glossary
```

### 13.4 Maintenance

- Sub-documents are source of truth
- Update sub-document first, then regenerate combined
- Version bump on any sub-document change

---

## Document History

| Version | Date | Changes |
|---------|------|---------|
| 1.0 | 2026-02-01 | Initial version from 3-question expansion analysis |

---

*Document Version: 1.0*
*Generated: 2026-02-01*
*Status: Approved for implementation*
*Methodology: 3-Question Expansion (24 questions, 24 answers)*
