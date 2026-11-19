# 1) Web: compila la SPA con Node 24.
FROM node:24 AS web
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# 2) Go: compila el binario con el build de la web ya embebido.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN rm -rf internal/web/dist
COPY --from=web /app/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 go build -trimpath -o /out/qa ./cmd/qa

# 3) Runtime: imagen mínima, sin root. Node, ocr y opencode se añaden en Fase 2.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 qa
COPY --from=build /out/qa /usr/local/bin/qa
USER qa
WORKDIR /home/qa
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/qa"]
