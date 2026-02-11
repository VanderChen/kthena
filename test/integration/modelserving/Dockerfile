# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build test binary
RUN CGO_ENABLED=0 GOOS=linux go test -c ./test/integration/modelserving -o /modelserving-test

# Runtime stage
FROM alpine:3.19

# Install kubectl for cluster operations
RUN apk add --no-cache kubectl

WORKDIR /test

# Copy test binary and test files
COPY --from=builder /modelserving-test .
COPY --from=builder /workspace/test/integration/modelserving/testcases ./testcases
COPY --from=builder /workspace/test/integration/modelserving/fixtures ./fixtures

# Create entrypoint script
RUN cat > /test/run-tests.sh << 'EOF'
#!/bin/sh
set -e

echo "================================================"
echo "ModelServing Integration Test Runner"
echo "================================================"
echo ""
echo "Kubernetes cluster: $(kubectl config current-context)"
echo "Namespace: ${TEST_NAMESPACE:-default}"
echo "Test filter: ${TEST_FILTER:-.*}"
echo ""

# Run the test binary
./modelserving-test \
  -test.v \
  -test.timeout=${TEST_TIMEOUT:-30m} \
  -test.run="${TEST_FILTER:-.*}"
EOF

RUN chmod +x /test/run-tests.sh

ENTRYPOINT ["/test/run-tests.sh"]
