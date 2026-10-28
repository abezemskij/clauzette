# --- build ------------------------------------------------------------------
FROM golang:1.26-bookworm AS build
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
FROM debian:bookworm-slim
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
ENV PATH=/usr/local/go/bin:/home/agent/go/bin:$PATH
USER 1000:1000
WORKDIR /workspace
ENV CLAUZETTE_CONFIG=/etc/clauzette/config.json
CMD ["clauzette", "idle"]
