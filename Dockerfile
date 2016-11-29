FROM golang:1.7.3-alpine

RUN mkdir -p "$GOPATH/src/github.com/panzerdev/k8s-redis-cluster-init"
WORKDIR $GOPATH/src/github.com/panzerdev/k8s-redis-cluster-init
ADD / .

RUN go install

ENTRYPOINT ["/go/bin/k8s-redis-cluster-init"]