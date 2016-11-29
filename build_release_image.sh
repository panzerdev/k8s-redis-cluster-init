#!/usr/bin/env bash

set -e

# first argument passed in is used for the image name
dockerTag=$1
echo $dockerTag
containerName=tmp_echo
docker rm $containerName || true
docker build -t $containerName .
# get build binary from container
id=$(docker create $containerName)
docker cp $id:/go/bin/k8s-redis-cluster-init .
docker rm -v $id

docker build -t $dockerTag -f Dockerfile.deploy .
rm k8s-redis-cluster-init
docker push $dockerTag