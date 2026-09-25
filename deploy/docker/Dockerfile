# The platform binary. One image, several personalities (see cmd/agentorch/main.go).
#
# Multi-stage, distroless-style final image: the thing that orchestrates
# untrusted code should itself have the smallest possible attack surface. There
# is no shell in the final image, so a command-injection bug in the control
# plane has nothing to injectd into.

FROM golang:1.24-bookworm AS build
WORKDIR /src
# Copy the module files first so dependency download is cached separately from
# source changes.
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# CGO_ENABLED=0 gives a static binary: it can be bind-mounted into a sandbox
# that has no shared libraries at all, which is how the `gh` and `aoconvert`
# personalities work.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/agentorch ./cmd/agentorch

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agentorch /usr/local/bin/agentorch
# Runs as uid 65532 (distroless "nonroot"). The namespace sandbox driver needs
# more privilege than this and is intended for the local demo; in Kubernetes
# the sandbox driver creates Pods instead, which needs no host privilege at all.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/agentorch"]
CMD ["serve"]
