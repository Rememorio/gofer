# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.5

FROM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath \
    -ldflags="-s -w -buildid= \
      -X github.com/Rememorio/gofer/internal/buildinfo.version=${VERSION} \
      -X github.com/Rememorio/gofer/internal/buildinfo.commit=${COMMIT} \
      -X github.com/Rememorio/gofer/internal/buildinfo.date=${BUILD_DATE}" \
    -o /out/gofer .

FROM alpine:3.22

RUN apk add --no-cache bash ca-certificates curl docker-cli git openssh-client tzdata \
    && addgroup -S -g 65532 gofer \
    && adduser -S -D -H -u 65532 -G gofer gofer \
    && install -d -o gofer -g gofer /var/lib/gofer

COPY --from=build /out/gofer /usr/local/bin/gofer

USER gofer:gofer
WORKDIR /var/lib/gofer
EXPOSE 8001

ENTRYPOINT ["gofer"]
CMD ["serve", "--config", "/etc/gofer/config.yaml"]
