# Build (context = repo root, needs go.mod): docker build -f cmd/orchestrator/Dockerfile -t quang47/llm-rca-orchestrator:v2.1 .
# cmd/orchestrator (incident-orchestrator) — groups Alertmanager
# webhooks into incidents, builds a ContextPack from ClickHouse +
# Prometheus, calls the LLM, stores the result. See deploy/rca/.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/orchestrator ./cmd/orchestrator
COPY internal/contextpack ./internal/contextpack
COPY internal/drain ./internal/drain
COPY internal/telemetry ./internal/telemetry
COPY internal/builder ./internal/builder
COPY internal/incident ./internal/incident
COPY internal/llm ./internal/llm
COPY internal/prompt ./internal/prompt
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/orchestrator ./cmd/orchestrator

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/orchestrator /orchestrator
ENTRYPOINT ["/orchestrator"]
