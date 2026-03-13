.PHONY: all build build-frontend build-backend build-docker clean dev

all: build

# Build everything
build: build-frontend build-backend

# Build the SvelteKit frontend
build-frontend:
	cd web && npm install && npm run build

# Build the Go backend
build-backend:
	go build -o kvm-switcher ./cmd/server/

# Build the JViewer Docker image (must be amd64 -- BMC native libs are x86_64 only)
build-docker:
	docker buildx build --platform linux/amd64 --load -t kvm-switcher/jviewer:latest docker/jviewer/

# Run the backend (requires frontend build + Docker image)
run: build
	./kvm-switcher -config configs/servers.yaml -web web/build

# Dev mode: run frontend and backend separately
dev-frontend:
	cd web && npm run dev

dev-backend:
	go run ./cmd/server/ -config configs/servers.yaml -web web/build

clean:
	rm -f kvm-switcher
	rm -rf web/build web/.svelte-kit
