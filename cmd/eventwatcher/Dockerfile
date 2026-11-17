# Build (context = repo root, needs go.mod): docker build -f cmd/eventwatcher/Dockerfile -t eventwatcher:local .
# Own eventwatcher binary (cmd/eventwatcher), built for local-only use on
# this cluster — no working ghcr.io mirror was found on the internal
# Nexus (nxrm-dso.mobifone.vn) for kubernetes-event-exporter, so we fall
# back to the project's own tool (kept in the repo for exactly this
# situation, see REPORT.md §9.1). Built and imported directly into one
# node's containerd, not pushed to any registry.
FROM nxrm-dso.mobifone.vn/dockerhub/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/eventwatcher ./cmd/eventwatcher
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/eventwatcher ./cmd/eventwatcher

# scratch instead of gcr.io/distroless/static: no working gcr.io mirror
# found on the internal Nexus proxy, and this binary needs no shell/libc
# anyway (CGO disabled, in-cluster config loads its own ca.crt path
# directly rather than relying on an OS trust store). Runs as root
# (scratch has no /etc/passwd for a numeric non-root UID to resolve
# against) — same tradeoff the original hostPath-based manifest made
# deliberately for the same PVC-ownership reason, see git history.
FROM scratch
COPY --from=build /out/eventwatcher /eventwatcher
ENTRYPOINT ["/eventwatcher"]
