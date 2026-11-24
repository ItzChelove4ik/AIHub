# Build context is the repository root (the backend imports OpenFlux from
# third_party/openflux):  docker build -f backend/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY third_party/openflux/go.mod third_party/openflux/go.sum third_party/openflux/
COPY backend/go.mod backend/go.sum backend/
RUN cd backend && go mod download
COPY third_party/openflux third_party/openflux
COPY backend backend
ARG VERSION=dev
RUN cd backend && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/aihub-backend ./cmd/aihub-backend \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/aihub-backend /usr/local/bin/aihub-backend
# The data volume must belong to the nonroot user (65532).
COPY --from=build --chown=65532:65532 /out/data /data
ENV AIHUB_LISTEN=:8080 AIHUB_DB_PATH=/data/aihub.db
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/aihub-backend", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/aihub-backend"]
