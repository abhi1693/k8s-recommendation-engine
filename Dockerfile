FROM golang:1.25 AS build

ARG TARGETOS=linux
ARG TARGETARCH=arm64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/k8s-recommendation-engine ./cmd/k8s-recommendation-engine

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --create-home --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot

COPY --from=build /out/k8s-recommendation-engine /usr/local/bin/k8s-recommendation-engine
ENV HOME=/home/nonroot
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/k8s-recommendation-engine"]
