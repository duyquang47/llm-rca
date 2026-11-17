# Build (context = repo root, needs go.mod): docker build -f cmd/faultctl/Dockerfile -t quang47/llm-rca-faultctl:TAG .
# cmd/faultctl — reads a scenario YAML, injects the fault for real
# (/_fault calls, direct k8s resource patches, Redis Stream poison
# messages), writes ground truth. See deploy/fault-manager/.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/faultctl ./cmd/faultctl
COPY internal/groundtruth ./internal/groundtruth
COPY internal/contextpack ./internal/contextpack
COPY internal/scenario ./internal/scenario
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/faultctl ./cmd/faultctl

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/faultctl /faultctl
ENTRYPOINT ["/faultctl"]
