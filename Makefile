.PHONY: web build test

# Construye la SPA y la deja lista para go:embed.
web:
	npm --prefix web ci
	npm --prefix web run build
	mkdir -p internal/web/dist
	cp -R web/dist/. internal/web/dist/

# Binario único con la web embebida.
build: web
	go build -o bin/qa ./cmd/qa

test:
	go test ./...
	npm --prefix web ci
	npm --prefix web run test -- --run
