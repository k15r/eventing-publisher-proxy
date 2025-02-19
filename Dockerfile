FROM europe-docker.pkg.dev/kyma-project/prod/external/golang:1.21.3-alpine3.18 as builder

ARG DOCK_PKG_DIR=/go/src/github.com/kyma-project/event-publisher-proxy

WORKDIR $DOCK_PKG_DIR
COPY . $DOCK_PKG_DIR

RUN CGO_ENABLED=0 GOOS=linux GO111MODULE=on go build -o event-publisher-proxy ./cmd/event-publisher-proxy

FROM gcr.io/distroless/static:nonroot
LABEL source = git@github.com:kyma-project/kyma.git
USER nonroot:nonroot

WORKDIR /
COPY --from=builder /go/src/github.com/kyma-project/event-publisher-proxy/event-publisher-proxy .


ENTRYPOINT ["/event-publisher-proxy"]
