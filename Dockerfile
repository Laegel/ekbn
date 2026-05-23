FROM golang:1.23-alpine AS builder

RUN apk add --no-cache nodejs npm

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build && \
    CGO_ENABLED=0 go build -o /ekbn        . && \
    CGO_ENABLED=0 go build -o /orchestrator ./cmd/orchestrator && \
    CGO_ENABLED=0 go build -o /plan        ./cmd/plan

FROM alpine:3.21

RUN apk add --no-cache ca-certificates libstdc++ libgcc

COPY --from=builder /ekbn        /usr/local/bin/ekbn
COPY --from=builder /orchestrator /usr/local/bin/orchestrator
COPY --from=builder /plan        /usr/local/bin/plan
COPY ekbn.config.yml custom.css /workspace/
COPY columns /workspace/columns

WORKDIR /workspace

EXPOSE 8080

ENTRYPOINT ["orchestrator"]
