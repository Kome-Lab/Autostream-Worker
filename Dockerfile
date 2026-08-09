FROM golang:1.26.5-trixie AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -o /out/autostream-worker -ldflags="-s -w -X github.com/example/autostream-worker/internal/version.Version=${VERSION} -X github.com/example/autostream-worker/internal/version.Commit=${COMMIT} -X github.com/example/autostream-worker/internal/version.BuildDate=${BUILD_DATE}" ./cmd/worker
RUN install -d -m 0750 -o 65532 -g 65532 /out/var/lib/autostream/worker

FROM gcr.io/distroless/base-debian13
COPY --from=build /out/autostream-worker /usr/local/bin/autostream-worker
COPY --from=build /out/autostream-worker /usr/local/bin/worker
COPY --from=build --chown=65532:65532 /out/var/lib/autostream/worker /var/lib/autostream/worker
ENV AUTOSTREAM_NODE_CONFIG=/etc/autostream-worker/config.yml
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/autostream-worker"]
