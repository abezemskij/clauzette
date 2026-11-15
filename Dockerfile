# --- build ------------------------------------------------------------------
FROM golang:1.27-trixie AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go vet ./... && go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/clauzette ./cmd/clauzette

# --- runtime ----------------------------------------------------------------
# A normal distribution on purpose: exec_command can only use what is
# installed here, so this image is the agent's toolbox. Add what your
# projects need (compilers, linters, ...) and remove what you don't want
# the agent to have (curl, for example, if it should not reach the network).
FROM debian:trixie-slim
# Links the image to its source repo (e.g. GHCR auto-associates the package).
LABEL org.opencontainers.image.source="https://github.com/abezemskij/clauzette"
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash ca-certificates curl diffutils file findutils git grep jq make \
      patch procps ripgrep sed tree unzip xz-utils \
      build-essential gdb \
      python3 python3-venv python3-pip \
 && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 1000 --create-home --home-dir /home/agent --shell /bin/bash agent
COPY --from=build /out/clauzette /usr/local/bin/clauzette
COPY --from=build /usr/local/go /usr/local/go
# Node.js — tracks the current LTS at build time (official static build).
# Pin a specific version here instead if you need reproducible builds.
RUN set -eux; \
    case "$(dpkg --print-architecture)" in \
      amd64) na=x64 ;; \
      arm64) na=arm64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac; \
    ver="$(curl -fsSL https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false)][0].version')"; \
    curl -fsSL "https://nodejs.org/dist/${ver}/node-${ver}-linux-${na}.tar.xz" -o /tmp/node.tar.xz; \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner; \
    rm /tmp/node.tar.xz; \
    node --version && npm --version
# npm's global prefix must live in the writable HOME (rootfs is read-only)
ENV NPM_CONFIG_PREFIX=/home/agent/.npm-global
ENV PATH=/usr/local/go/bin:/home/agent/go/bin:/home/agent/.npm-global/bin:$PATH
USER 1000:1000
WORKDIR /workspace
ENV CLAUZETTE_CONFIG=/etc/clauzette/config.json
CMD ["clauzette", "idle"]
