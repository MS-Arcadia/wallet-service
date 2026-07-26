# Wallet service image.
#
# Two things shape this Dockerfile. First, the service depends on the shared platform
# module from the infra repository, which is resolved through a `replace` directive to a
# sibling checkout — so the build context has to contain both repositories. Build it
# from the parent directory:
#
#   docker build -f wallet-service/Dockerfile -t arcadia/wallet-service .
#
# or let the compose file in infra/deploy/compose do it for you.
#
# Second, the result is a distroless image running as a non-root user with no shell.
# A wallet service is the highest-value target in the platform; there is no reason for
# its container to contain a package manager an attacker could use.

# --- Build stage -----------------------------------------------------------
FROM golang:1.24-alpine AS builder

# git is needed for module resolution; ca-certificates so the build can reach a proxy.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy only the module files first. This layer is cached as long as dependencies do not
# change, which is what keeps a code-only rebuild fast.
COPY infra/platform/go.mod infra/platform/go.sum ./infra/platform/
COPY wallet-service/go.mod wallet-service/go.sum ./wallet-service/

WORKDIR /src/wallet-service
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Now the sources.
COPY infra/platform ../infra/platform
COPY wallet-service .

ARG VERSION=dev
ARG COMMIT=unknown

# CGO_ENABLED=0 produces a static binary, which is what allows the distroless base.
# -trimpath keeps absolute build paths out of the binary; -s -w drop the symbol table
# and DWARF data, which are of no use in production and only help somebody reversing it.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/wallet-service ./cmd/wallet-service

# --- Runtime stage ---------------------------------------------------------
# Distroless: no shell, no package manager, no busybox. If the process is compromised
# there is nothing in the image to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot

# Copied for completeness: the service itself makes no outbound TLS calls today, but the
# payment adapter client will once a real gateway is configured behind TLS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /out/wallet-service /usr/local/bin/wallet-service

# 65532 is distroless's `nonroot` user. Running as root inside a container buys nothing
# and turns a container escape into a host compromise.
USER 65532:65532

# 8080 serves REST plus /metrics, /livez and /readyz; 9090 serves gRPC.
EXPOSE 8080 9090

# No HEALTHCHECK directive on purpose: Kubernetes owns liveness and readiness through
# the probes on /livez and /readyz, and a duplicate Docker-level check would only add a
# second, differently-configured opinion.

ENTRYPOINT ["/usr/local/bin/wallet-service"]
