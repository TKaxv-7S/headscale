#!/bin/bash

SHELL_PATH=$(dirname $(readlink -f "$0"))

#测试
#docker run --rm -it nixos/nix

docker run --rm -it -v "$SHELL_PATH"/:/workdir -v "$SHELL_PATH"/nix_cache:/root/.cache/nix:Z nixos/nix

#进入容器中执行
#cd /workdir
#export CGO_ENABLED=1

#如果已有缓存无需执行
#nix develop --extra-experimental-features 'nix-command flakes'
#go mod download
#go mod tidy


#make generate

#make test

#make build
