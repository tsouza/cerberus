# === Build ===

# Build cerberus into ./bin.
build:
    go build -trimpath -o bin/cerberus ./cmd/cerberus

# Install cerberus into $GOBIN.
install:
    go install -trimpath ./cmd/cerberus

# Remove build outputs.
clean:
    rm -rf bin/ dist/

