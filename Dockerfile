# 1) Build the web UI
FROM node:22-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# 2) Build the Go server with the UI embedded
FROM golang:1.25-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/embed.go ./web/embed.go
COPY --from=ui /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/authz-lab ./cmd/server

# 3) Minimal runtime image
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /out/authz-lab /authz-lab
# Render injects PORT; 10000 is its default.
ENV PORT=10000
EXPOSE 10000
USER nonroot:nonroot
ENTRYPOINT ["/authz-lab"]
