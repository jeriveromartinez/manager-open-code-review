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

# 3) Worker: Node 24 aporta ocr y opencode; git se usa para los checkouts.
#    Trixie trae git >= 2.41, mínimo que exige ocr. Go y bubblewrap ejecutan la
#    evidencia diferencial base–head dentro de un sandbox (#230).
FROM node:24-trixie-slim AS worker
RUN apt-get update \
    && apt-get install -y --no-install-recommends bubblewrap ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && npm i -g @alibaba-group/open-code-review@1.14.1 opencode-ai@1.19.0 \
    && useradd --create-home --uid 10001 qa
COPY --from=build /out/qa /usr/local/bin/qa
# The differential executor runs `go test` inside bubblewrap; CGO stays off.
COPY --from=build /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
USER qa
WORKDIR /home/qa
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/qa"]
CMD ["worker"]

# 4) App (target por defecto): runtime mínimo sin Node; sólo la app recibe
#    QA_MASTER_KEY. El worker usa otro target y nunca ve esa variable.
FROM debian:bookworm-slim AS app
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 qa
COPY --from=build /out/qa /usr/local/bin/qa
USER qa
WORKDIR /home/qa
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/qa"]
