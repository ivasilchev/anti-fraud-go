# Bare-bones image: the three static binaries on distroless.
# Build with `make docker-build`, run with `make docker-up` (scripts/docker.sh).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/server ./cmd/generate ./cmd/verify

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /usr/local/bin/
ENV DB_PATH=/data/events.db SNAPSHOT_PATH=/data/snapshot.bin
EXPOSE 8080
HEALTHCHECK --start-period=10m --start-interval=1s CMD ["/usr/local/bin/server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/server"]
