# Build (context = repo root, needs go.mod): docker build -f cmd/evaluator/Dockerfile -t quang47/llm-rca-evaluator:TAG .
# cmd/evaluator — matches an incident against ground truth, scores
# top-1/MRR (or recall for multi-cause scenarios), runs
# context_assertions, writes scores to ClickHouse. See deploy/fault-manager/.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/evaluator ./cmd/evaluator
COPY internal/contextpack ./internal/contextpack
COPY internal/groundtruth ./internal/groundtruth
COPY internal/llm ./internal/llm
COPY internal/scenario ./internal/scenario
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/evaluator ./cmd/evaluator

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/evaluator /evaluator
ENTRYPOINT ["/evaluator"]
