FROM golang:1.24-alpine AS builder

RUN apk add --no-cache nodejs npm

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build && \
    CGO_ENABLED=0 go build -o /ekbn .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates libstdc++ libgcc

COPY --from=builder /ekbn /usr/local/bin/ekbn
COPY ekbn.config.yml custom.css /workspace/

WORKDIR /workspace

EXPOSE 8080

# Headless service mode (no TUI — that needs a real TTY): starts the
# orchestrator's polling loop plus the in-process HTTP kanban UI. For the
# interactive TUI in a container, override with:
#   docker run -it <image> ekbn
ENTRYPOINT ["ekbn", "orchestrator"]
