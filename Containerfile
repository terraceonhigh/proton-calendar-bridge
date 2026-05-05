FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build
RUN go build -o /out/proton-calendar-bridge ./cmd/proton-calendar-bridge/

# Test stage (same image, run tests with coverage)
FROM builder AS tester
RUN go vet ./...
CMD ["go", "test", "-v", "-count=1", "-coverprofile=/out/coverage.out", "./..."]
