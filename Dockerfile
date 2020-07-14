ARG BUILDPLATFORM

FROM --platform=$BUILDPLATFORM golang:1.14

ARG TARGETARCH
ARG TARGETOS

ENV GO111MODULE=on
WORKDIR /go/src/github.com/wish/nodetaint

# Cache dependencies
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . /go/src/github.com/wish/nodetaint/

# Build controller
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} GOOS=${TARGETOS} go build -o . -a -installsuffix cgo .

FROM alpine:3.7
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=0 /go/src/github.com/wish/nodetaint/nodetaint /root/nodetaint
