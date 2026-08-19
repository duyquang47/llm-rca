# Single image for all six service-chain deployments — behavior is
# determined entirely by the mounted ConfigMap. See CLAUDE.md §4.5, §13.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/chain ./cmd/chain
COPY internal/chain ./internal/chain
COPY pkg/otelinit ./pkg/otelinit
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/chain ./cmd/chain

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/chain /chain
ENTRYPOINT ["/chain"]
