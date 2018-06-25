FROM alpine:3.6

RUN apk update && apk add ca-certificates && \
    apk add tzdata && \
    ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

RUN mkdir -p /opt/mlkube
COPY tf-operator /opt/mlkube/tf-operator
ENTRYPOINT ["/opt/mlkube/tf-operator", "-alsologtostderr"]
