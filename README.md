# gmail-worker-migla

Lightweight Go worker that listens to Gmail Pub/Sub notifications and forwards them to Payload backend REST API.

## Features

- Pulls messages from `GMAIL_PUBSUB_SUBSCRIPTION`
- Retries backend calls with exponential backoff (`MAX_HTTP_RETRIES`, `BACKOFF_*`)
- Dead-letters poison messages to `GMAIL_DEAD_LETTER_TOPIC` after `MAX_DELIVERY_ATTEMPT`
- Sends `X-Pubsub-Message-Id` header for backend idempotency guard
- Exposes `/healthz` for container platforms (default `PORT=8080`)

## Image build (Hetzner VM + Watchtower flow)

This repo follows your current pattern: build image locally, push to registry, let Watchtower pull `latest` on VM.

Default image settings are in `Makefile`:

- `REGISTRY=your-docker-repo`
- `IMAGE=name-it-yours`
- `PLATFORM=linux/amd64` for mac M chip series
- `VERSION=<git short sha>` (or pass explicitly)

Build and push:

```bash
make docker_build
```

Build and push with explicit version:

```bash
make docker_build VERSION=1.0.0
```

Build locally without push:

```bash
make docker_build_local VERSION=dev
```

## Environment variables

- `GCP_PROJECT_ID`
- `GMAIL_PUBSUB_SUBSCRIPTION`
- `GMAIL_DEAD_LETTER_TOPIC` (optional but recommended)
- `PAYLOAD_INFO_BOT_ENDPOINT_URL` (Payload endpoint URL, typically `/api/internal/gmail/info-bot/notification`)
- `GMAIL_WORKER_SHARED_SECRET` (must match backend `GMAIL_WORKER_SHARED_SECRET`)
- `HTTP_TIMEOUT_SECONDS` (default: `12`)
- `MAX_HTTP_RETRIES` (default: `4`)
- `BACKOFF_INITIAL_MS` (default: `500`)
- `BACKOFF_MAX_MS` (default: `8000`)
- `MAX_DELIVERY_ATTEMPT` (default: `12`)

## Run locally

```bash
cp .env.example .env
# edit .env with real values

set -a
source .env
set +a

go mod tidy
go run .
```

One-liner:

```bash
set -a && source .env && set +a && go run .
```

## Backend requirement (idempotency)

Backend endpoint must enforce:

- `Authorization: Bearer <GMAIL_WORKER_SHARED_SECRET>`
- `X-Pubsub-Message-Id` header
- unique record per `pubsubMessageId` to skip duplicates safely
