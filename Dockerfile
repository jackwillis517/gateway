# ---- build stage ----
FROM golang:1.27-alpine AS build

WORKDIR /src

# Cache the module download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /bin/api ./cmd/api

# ---- runtime stage ----
# distroless/static ships ca-certificates + tzdata (needed for upstream TLS
# calls) and no shell/package surface. The :nonroot tag runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /bin/api /api

EXPOSE 4000
USER nonroot:nonroot

ENTRYPOINT ["/api"]