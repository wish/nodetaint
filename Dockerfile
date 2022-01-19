FROM --platform=$BUILDPLATFORM golang:1.17

ARG BUILDPLATFORM
ARG TARGETARCH=amd64
ARG TARGETOS=linux

ENV GO111MODULE=on
WORKDIR /go/src/github.com/wish/nodetaint

# Cache dependencies
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . /go/src/github.com/wish/nodetaint/

RUN go mod tidy
# Build controller
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} GOOS=${TARGETOS} go build -o . -a -installsuffix cgo .

FROM alpine:3.15
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=0 /go/src/github.com/wish/nodetaint/nodetaint /root/nodetaint
ENTRYPOINT /root/nodetaint
