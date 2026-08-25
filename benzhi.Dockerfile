FROM --platform=$BUILDPLATFORM golang:1.22 AS build
WORKDIR /app
ARG GOPROXY=https://goproxy.cn,direct
ARG TARGETOS
ARG TARGETARCH
ENV GOPROXY=$GOPROXY
COPY go.mod go.sum ./
RUN --mount=type=cache,id=benzhi-go-mod,target=/go/pkg/mod     GOTOOLCHAIN=local go mod download
COPY . .
RUN --mount=type=cache,id=benzhi-go-mod,target=/go/pkg/mod     --mount=type=cache,target=/root/.cache/go-build     CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOTOOLCHAIN=local go build ./...

FROM --platform=$TARGETPLATFORM golang:1.22
WORKDIR /app
COPY --from=build /app /app
CMD ["bash"]
