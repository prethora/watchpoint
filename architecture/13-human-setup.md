# 13 - Human Setup & Bootstrap Guide

> **Purpose**: Defines the protocol for an AI Agent to guide a Human Operator through the manual bootstrapping process. This includes account creation, secret harvesting, and the "chicken-and-egg" dependencies required before the first Infrastructure-as-Code deployment.
> **Package**: Operations / Bootstrap
> **Dependencies**: `03-config.md` (SSM Keys), `04-sam-template.md` (Outputs), `12-operations.md` (Scripts)

---

## Table of Contents

1. [Overview & Agent Protocol](#1-overview--agent-protocol)
2. [Session Initialization](#2-session-initialization)
3. [Phase 1: The Vault (External Accounts)](#3-phase-1-the-vault-external-accounts)
4. [Phase 2: SSM Injection (The Glue)](#4-phase-2-ssm-injection-the-glue)
5. [Phase 3: Infrastructure Deployment](#5-phase-3-infrastructure-deployment)
6. [Phase 4: Artifact Bootstrap (RunPod)](#6-phase-4-artifact-bootstrap-runpod)
7. [Phase 5: Post-Deployment Wiring](#7-phase-5-post-deployment-wiring)
8. [Final Validation](#8-final-validation)
9. [Teardown Protocol](#9-teardown-protocol)

---

## 1. Overview & Agent Protocol

This document is written for the **AI Agent** executing the setup. It defines the prompts, validation logic, and sequence required to bootstrap the WatchPoint platform.

### Agent Directives

1.  **Security Hygiene**: When prompting for `SecureString` inputs (API Keys, Secrets), **NEVER** echo the user's input back to the console or logs. Acknowledge receipt with a length check (e.g., "Received 32 chars").
2.  **Idempotency**: Before asking for a value, always probe AWS SSM (`aws ssm get-parameter`) to see if it already exists. If found, ask to **[Skip]** or **[Overwrite]**.
3.  **Validation**: Always validate input syntax (Regex) or function (Curl) before accepting it. Fail fast.
4.  **Context**: Maintain the `Environment` (dev/staging/prod) and `AWS_PROFILE` context throughout the session.
5.  **Optional Parameters / Incremental Setup**: The bootstrap tool supports skipping any prompted parameter by pressing Enter (empty input) and then confirming with **[S]kip**. This enables partial or incremental setup -- for example, OAuth credentials (Google/GitHub) are only needed if the operator wants dashboard login and can be omitted for API-key-based developer access. Auto-generated parameters (Session Key, Admin API Key) and fixed-value parameters are always populated and cannot be skipped. Because the tool is fully idempotent (Directive 2), skipped parameters can be populated in a subsequent bootstrap run without affecting previously configured values.

### Interaction Schema

The guide uses the following schema for AI-to-Human interactions:

| Field | Description |
| :--- | :--- |
| **Context** | Why this step is necessary. |
| **Prerequisite** | What the human must have open/ready. |
| **Prompt** | The exact text to display to the user. |
| **Validation** | Logic (Regex/CLI) the AI must run to verify input. |
| **Action** | The system command the AI executes upon success. |

---

## 2. Session Initialization

### 2.1 Prerequisite Check

**Context**: Verify the local machine has the required toolchain.

**Action**: Run the following commands. Halt if any fail or version is insufficient.

| Tool | Command | Min Version |
| :--- | :--- | :--- |
| AWS CLI | `aws --version` | 2.x |
| SAM CLI | `sam --version` | 1.x |
| Docker | `docker --version` | 20.x |
| Go | `go version` | 1.21 |
| Python | `python3 --version` | 3.11 |

### 2.2 Context Selection

**Prompt**:
> "Initializing WatchPoint Bootstrap.
> 1. Target Environment (dev/staging/prod): "
> 2. AWS Profile (default): "

**Action**: Store inputs as `CTX_ENV` and `CTX_PROFILE`. Interpolate these into all subsequent commands (e.g., `/{CTX_ENV}/watchpoint/...`).

---

## 3. Phase 1: The Vault (External Accounts)

**Goal**: Create external accounts and harvest secrets.
**Strategy**: Instruct the user to keep browser tabs open.

### 3.1 Supabase (Database)

*   **Context**: Primary PostgreSQL database.
*   **Prompt**:
    > "1. Create a new Supabase Project.
    > 2. Go to **Settings > Database**.
    > 3. Under **Connection Pooling**, set Mode to **Transaction**.
    > 4. Copy the Connection String (port 6543) and replace the password.
    > 5. Paste the full `postgres://...` string here:"
*   **Validation**: Regex `^postgres:\/\/.*:6543\/.*$`
*   **Store**: `VAR_DB_URL`

### 3.2 Stripe (Billing)

*   **Context**: Subscription management.
*   **Prompt**:
    > "1. Go to Stripe Dashboard > Developers > API Keys.
    > 2. Copy the **Secret Key** (`sk_...`).
    > 3. Paste it here:"
*   **Validation**: Regex `^sk_(test|live)_[0-9a-zA-Z]{24,}$`
*   **Store**: `VAR_STRIPE_SECRET`
*   **Secondary Prompt**: "Now paste the **Publishable Key** (`pk_...`):"
*   **Store**: `VAR_STRIPE_PUB`

### 3.3 SendGrid (Email)

*   **Context**: Transactional email delivery.
*   **Prompt**:
    > "1. Go to SendGrid > Settings > API Keys > Create API Key (Full Access).
    > 2. Paste the Key (`SG...`) here:"
*   **Validation**: `curl -s -H "Authorization: Bearer $INPUT" https://api.sendgrid.com/v3/user/credits | grep "remain"`
*   **Store**: `VAR_SENDGRID_KEY`
*   **Blocking Step**:
    > "Go to Settings > Sender Authentication. Verify a **Single Sender** (e.g., `alerts@watchpoint.io`). I will wait until this is verified."
    *   *Action*: Poll `GET /v3/verified_senders` every 30s until valid.

### 3.4 RunPod (GPU Inference)

*   **Context**: Serverless GPU forecasting.
*   **Prompt**:
    > "1. Go to RunPod > Settings > API Keys.
    > 2. Create and paste a new API Key:"
*   **Validation**: Length > 20.
*   **Store**: `VAR_RUNPOD_KEY`

### 3.5 OAuth Providers (Identity)

*   **Context**: User login.
*   **Instruction**: "Use `https://placeholder.watchpoint.io` as the Authorized Redirect URI for now. We will update this after deployment."
*   **Prompt**: "Paste Google Client ID:" -> `VAR_GOOGLE_ID`
*   **Prompt**: "Paste Google Client Secret:" -> `VAR_GOOGLE_SECRET`
*   **Prompt**: "Paste GitHub Client ID:" -> `VAR_GITHUB_ID`
*   **Prompt**: "Paste GitHub Client Secret:" -> `VAR_GITHUB_SECRET`

---

## 4. Phase 2: SSM Injection (The Glue)

**Goal**: Push collected variables into AWS Systems Manager Parameter Store.

**Mechanism**: The Agent iterates through the **Secret Inventory**. For each item:
1.  Check if parameter exists (`aws ssm get-parameter`).
2.  If exists, skip (unless `--force`).
3.  If missing, use the cached `VAR_*` from Phase 1.
4.  Run `aws ssm put-parameter`.

### Secret Inventory Table

| Human Label | SSM Path Template | Type | Source Variable |
| :--- | :--- | :--- | :--- |
| Database URL | `/{env}/watchpoint/database/url` | SecureString | `VAR_DB_URL` |
| Stripe Secret | `/{env}/watchpoint/billing/stripe_secret_key` | SecureString | `VAR_STRIPE_SECRET` |
| Stripe Public | `/{env}/watchpoint/billing/stripe_publishable_key` | String | `VAR_STRIPE_PUB` |
| SendGrid Key | `/{env}/watchpoint/email/sendgrid_api_key` | SecureString | `VAR_SENDGRID_KEY` |
| RunPod Key | `/{env}/watchpoint/forecast/runpod_api_key` | SecureString | `VAR_RUNPOD_KEY` |
| Google Secret | `/{env}/watchpoint/auth/google_secret` | SecureString | `VAR_GOOGLE_SECRET` |
| GitHub Secret | `/{env}/watchpoint/auth/github_secret` | SecureString | `VAR_GITHUB_SECRET` |
| Session Key | `/{env}/watchpoint/auth/session_key` | SecureString | *(Generate Random 32b)* |
| Admin API Key | `/{env}/watchpoint/security/admin_api_key` | SecureString | *(Generate Random 32b)* |

---

## 5. Phase 3: Infrastructure Deployment

**Goal**: Deploy the SAM template to create API Gateway, ECR, and Buckets.

**Prompt**:
> "Secrets configured. Ready to deploy infrastructure.
> Please run: `sam build && sam deploy --config-env {CTX_ENV} --guided`
>
> **CRITICAL**: When asked for `RunPodEndpointId`, enter `pending_setup`. We will update this in Phase 4."

**Action**: Wait for user confirmation of deployment success.

**Harvest Outputs**:
Ask the user to paste the CloudFormation Outputs:
*   `ApiEndpoint`: (e.g., `https://xyz.execute-api.us-east-1.amazonaws.com`)
*   `EvalWorkerRepositoryUri`: (e.g., `123.dkr.ecr...`)

---

## 6. Phase 4: Artifact Bootstrap (RunPod)

**Goal**: Build the inference image, push to ECR, deploy RunPod Endpoint, and update config.

### 6.1 Build & Push
**Prompt**:
> "We must now deploy the inference container. Run these commands:
> 1. `aws ecr get-login-password | docker login --username AWS --password-stdin {EvalWorkerRepositoryUri}`
> 2. `docker build -t {EvalWorkerRepositoryUri}:latest -f runpod/Dockerfile .`
> 3. `docker push {EvalWorkerRepositoryUri}:latest`"

### 6.2 Model Weight Sideloading (The "Cold Start" Fix)
**Context**: The inference worker requires massive model weights (~50GB) that are not baked into the Docker image. We must stage them in S3 so the worker can download them on startup.

**Prompt**:
> "⚠️ **Manual Action Required: Model Weights**
> 1. Log in to [NVIDIA NGC](https://catalog.ngc.nvidia.com/) (or your model source).
> 2. Download the `.pt` or `.onnx` files for **Atlas** and **StormScope**.
> 3. **Action**: Run the helper script to upload them to your artifact bucket:
>    `./scripts/upload-weights.sh --bucket {ArtifactBucketName} --atlas /path/to/atlas.pt --nowcast /path/to/stormscope.pt`
>
> *Note: If you do not have the real weights yet, the script can generate dummy weights for testing if you pass `--mock`.*"

**Validation**: Check `aws s3 ls s3://{ArtifactBucketName}/weights/` contains the files.

### 6.3 Create Endpoint
**Prompt**:
> "1. Go to RunPod > Serverless > New Endpoint.
> 2. Image: `{EvalWorkerRepositoryUri}:latest`
> 3. Container Disk: 10 GB
> 4. GPU: NVIDIA RTX 4090 or similar (24GB VRAM required).
> 5. Create Endpoint.
> 6. Copy the **Endpoint ID** (e.g., `vllm-xyz`) and paste it here:"

**Validation**: Regex `^[a-zA-Z0-9-]{10,}$`

### 6.4 Update Config
**Action**:
1.  Update CloudFormation Parameter `RunPodEndpointId` (via `sam deploy` or SSM override if architecture permits).
    *   *Decision*: We map `RunPodEndpointId` to an SSM parameter `/{env}/watchpoint/forecast/runpod_endpoint_id` to avoid redeploying the stack.
2.  Run `aws ssm put-parameter --name /{env}/watchpoint/forecast/runpod_endpoint_id --value {INPUT} --type String --overwrite`.

---

## 7. Phase 5: Post-Deployment Wiring

**Goal**: Update "Chicken-and-Egg" URLs in external vendors using the real `ApiEndpoint`.

### Patch Manifest

The Agent iterates through this list, generating specific instructions:

| Vendor | Field to Update | Value Construction |
| :--- | :--- | :--- |
| **Stripe** | Developers > Webhooks > Add Endpoint | `{ApiEndpoint}/v1/webhooks/stripe` |
| **Google Cloud** | OAuth Consent > Authorized Redirect URIs | `{ApiEndpoint}/auth/oauth/google/callback` |
| **GitHub** | Developer Settings > OAuth Apps > Callback URL | `{ApiEndpoint}/auth/oauth/github/callback` |

**Prompt**:
> "For each vendor below, replace the placeholder URL with the real values shown."

### Stripe Product Seeding
**Prompt**:
> "Initializing Stripe Products (Starter, Pro, Business).
> Running: `go run cmd/ops/seed-stripe/main.go`"

**Action**: Execute script. Validate exit code 0.

---

## 8. Final Validation

**Goal**: Verify the platform is operational.

### 8.1 Smoke Test
**Action**: Invoke the Data Poller manually to trigger a pipeline check.
`aws lambda invoke --function-name data-poller-{CTX_ENV} --payload '{"force_retry": true}' response.json`

**Validation**:
1.  Check `response.json` for success.
2.  Check CloudWatch Metric `WatchPoint/ForecastReady`.

### 8.2 Status Report
Generate a markdown summary:

| Component | Status | Notes |
| :--- | :--- | :--- |
| **Infrastructure** | 🟢 Deployed | API: `{ApiEndpoint}` |
| **Database** | 🟢 Connected | Transaction Mode Active |
| **Secrets** | 🟢 Populated | 9 Parameters |
| **RunPod** | 🟢 Configured | ID: `{RunPodID}` |
| **Stripe** | 🟢 Wired | Webhook Active |

**Prompt**: "🚀 Platform Ready. You can now use the API."

---

## 9. Teardown Protocol

**Context**: **CRITICAL**. Running `sam delete` does **NOT** stop billing for external services.

**Prompt**:
> "⚠️ **TEARDOWN WARNING** ⚠️
> If you destroy this stack, you **MUST** manually delete the following to avoid zombie bills:
> 1. **RunPod**: Delete the Serverless Endpoint (Hourly GPU charges).
> 2. **Stripe**: Cancel any test subscriptions created.
> 3. **Supabase**: Pause/Delete the project.
> 4. **AWS ECR**: The repository may contain images that cost storage."
