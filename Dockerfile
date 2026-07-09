FROM golang:1.25 AS build

ARG TARGETOS=linux
ARG TARGETARCH=arm64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/k8s-recommendation-engine ./cmd/k8s-recommendation-engine

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/k8s-recommendation-engine /usr/local/bin/k8s-recommendation-engine
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/k8s-recommendation-engine"]
