FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build

ARG TARGETOS
ARG TARGETARCH
ARG APP_VERSION=dev
ENV GOTOOLCHAIN=local

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${APP_VERSION}" -o /out/k8s-recommendation-engine ./cmd/k8s-recommendation-engine

FROM alpine:3.24@sha256:71e9b7bae160ba167fa17414401b94cbcdaf3c32feeab9298bd794a2a0614c35 AS runtime

RUN apk upgrade --no-cache && apk add --no-cache ca-certificates git \
    && addgroup -S -g 65532 nonroot \
    && adduser -S -D -u 65532 -G nonroot -h /home/nonroot -s /sbin/nologin nonroot

COPY --from=build /out/k8s-recommendation-engine /usr/local/bin/k8s-recommendation-engine
ARG APP_VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/abhi1693/k8s-recommendation-engine" \
      org.opencontainers.image.version=$APP_VERSION
ENV HOME=/home/nonroot
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/k8s-recommendation-engine"]
