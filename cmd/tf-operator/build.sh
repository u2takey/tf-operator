#!/usr/bin/env bash

set -o errexit
set -o pipefail

IMAGE_DEST="${IMAGE_DEST:-ccr.ccs.tencentyun.com/mla-library/tf_operator:v0.13}"

usage() {
	echo "Usage: "
    echo "	build  : build"
	echo "	build-and-push  : build and push to IMAGE_DEST"
	echo "Options [Env]:"
	echo "	IMAGE_DEST    : [${IMAGE_DEST}] "
	exit 1
}

build() {
    GOOS=linux GOARCH=amd64 go build .
}

build-and-push() {
    GOOS=linux GOARCH=amd64 go build .
    docker build -t ${IMAGE_DEST} -f ./Dockerfile .
    docker push ${IMAGE_DEST} 
}

op=$1

case "$op" in
	build)
		build
		;;
	build-and-push)
		build-and-push 
		;;
	*)
		usage
		exit 0
esac
 
