GOOS=linux   GOARCH=amd64 go build .
docker build -t ccr.ccs.tencentyun.com/mla-library/tf_operator:v0.11 .
