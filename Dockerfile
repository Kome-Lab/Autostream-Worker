FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -o /out/worker ./cmd/worker

FROM gcr.io/distroless/base-debian13
COPY --from=build /out/worker /usr/local/bin/worker
ENV AUTOSTREAM_NODE_CONFIG=/etc/autostream-node/config.yml
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/worker"]
