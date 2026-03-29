#!/usr/bin/env bash
# Run E2E tests against a mock BMC server (no real hardware needed).
# This script is designed for CI environments but can also be run locally.
#
# Usage: bash tests/e2e/run-ci.sh
#
# Required env vars:
#   MOCK_BMC_PASSWORD - password for mock BMC auth (any value works)

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
E2E_DIR="$ROOT_DIR/tests/e2e"
MOCK_BMC_HTTP_PORT=9999
MOCK_BMC_HTTPS_PORT=9998
APP_PORT=8081
MOCK_BMC_PID=""
APP_PID=""

cleanup() {
    echo "Cleaning up..."
    if [ -n "$MOCK_BMC_PID" ] && kill -0 "$MOCK_BMC_PID" 2>/dev/null; then
        kill "$MOCK_BMC_PID" 2>/dev/null || true
        wait "$MOCK_BMC_PID" 2>/dev/null || true
    fi
    if [ -n "$APP_PID" ] && kill -0 "$APP_PID" 2>/dev/null; then
        kill "$APP_PID" 2>/dev/null || true
        wait "$APP_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Ensure MOCK_BMC_PASSWORD is set
export MOCK_BMC_PASSWORD="${MOCK_BMC_PASSWORD:-admin}"

# Step 1: Build the mock BMC server
echo "==> Building mock BMC server..."
cd "$ROOT_DIR"
go build -o "$E2E_DIR/mockbmc/mockbmc" ./tests/e2e/mockbmc/

# Step 2: Build the app server
echo "==> Building app server..."
go build -o "$ROOT_DIR/server-e2e" ./cmd/server/

# Step 3: Check frontend is built
if [ ! -d "$ROOT_DIR/web/build" ]; then
    echo "ERROR: Frontend not built. Run 'cd web && npm ci && npm run build' first."
    exit 1
fi

# Step 4: Start mock BMC (serves HTTP on 9999, HTTPS on 9998)
echo "==> Starting mock BMC (HTTP :$MOCK_BMC_HTTP_PORT, HTTPS :$MOCK_BMC_HTTPS_PORT)..."
"$E2E_DIR/mockbmc/mockbmc" -port "$MOCK_BMC_HTTP_PORT" -tls-port "$MOCK_BMC_HTTPS_PORT" &
MOCK_BMC_PID=$!

# Step 5: Start the app server with test config
echo "==> Starting app server on port $APP_PORT..."
"$ROOT_DIR/server-e2e" \
    -config "$E2E_DIR/configs/test-servers.yaml" \
    -web "$ROOT_DIR/web/build" &
APP_PID=$!

# Step 6: Wait for both servers to be ready
echo "==> Waiting for servers to be ready..."
for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:$MOCK_BMC_HTTP_PORT/" > /dev/null 2>&1; then
        echo "    Mock BMC HTTP is ready"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: Mock BMC failed to start"
        exit 1
    fi
    sleep 1
done

for i in $(seq 1 30); do
    if curl -sf -k "https://127.0.0.1:$MOCK_BMC_HTTPS_PORT/" > /dev/null 2>&1; then
        echo "    Mock BMC HTTPS is ready"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: Mock BMC HTTPS failed to start"
        exit 1
    fi
    sleep 1
done

for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:$APP_PORT/healthz" > /dev/null 2>&1; then
        echo "    App server is ready"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: App server failed to start"
        exit 1
    fi
    sleep 1
done

# Step 7: Run Playwright tests
echo "==> Running E2E tests..."
cd "$E2E_DIR"
npx playwright test ci.spec.ts --project=ci
TEST_EXIT=$?

echo "==> E2E tests finished with exit code $TEST_EXIT"
exit $TEST_EXIT
