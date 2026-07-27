# Build:  docker build -t arcadia/wallet-service:local .
#
# The context is this repository and nothing else. The service vendors everything it
# needs under internal/, so there is no sibling checkout to arrange and no shared module
# to resolve.
#
# The result is a distroless image running as a non-root user with no shell: if the
# process is compromised there is nothing in the image to pivot with.

FROM golang:1.24-alpine AS build
WORKDIR /src

# Dependencies first, so a code-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev

# CGO_ENABLED=0 gives a static binary, which is what allows the distroless base.
# -trimpath keeps build paths out of the binary; -s -w drop symbols that only help
# somebody reversing it.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/wallet-service ./cmd/wallet-service

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wallet-service /usr/local/bin/wallet-service

# 65532 is distroless's nonroot user. Running as root in a container buys nothing and
# turns a container escape into a host compromise.
USER 65532:65532

# 8080 serves REST plus /metrics, /livez and /readyz; 9090 serves gRPC.
EXPOSE 8080 9090

# The exec form is required: this image has no shell for the usual CMD-SHELL form to
# run, which is why the binary probes itself. start-period covers migrations, which run
# at boot before the listener opens.
HEALTHCHECK --interval=15s --timeout=5s --start-period=40s --retries=3 \
  CMD ["/usr/local/bin/wallet-service", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/wallet-service"]
