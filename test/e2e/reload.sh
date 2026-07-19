#!/usr/bin/env bash
set -eo pipefail

CLUSTER_NAME="kube-autopsy-e2e"
IMAGE_NAME="kube-autopsy:e2e"
NAMESPACE="kube-autopsy"

echo "━━━ Reloading kube-autopsy into kind cluster ━━━"

echo "[1/3] Building new Docker image..."
docker build -t ${IMAGE_NAME} .

echo "[2/3] Loading image into kind cluster '${CLUSTER_NAME}'..."
kind load docker-image ${IMAGE_NAME} --name ${CLUSTER_NAME}

echo "[3/3] Restarting components to pick up new image..."
kubectl --context kind-${CLUSTER_NAME} -n ${NAMESPACE} rollout restart daemonset/kube-autopsy-agent
kubectl --context kind-${CLUSTER_NAME} -n ${NAMESPACE} rollout restart deployment/kube-autopsy-controller

echo "Waiting for rollouts to complete..."
kubectl --context kind-${CLUSTER_NAME} -n ${NAMESPACE} rollout status daemonset/kube-autopsy-agent
kubectl --context kind-${CLUSTER_NAME} -n ${NAMESPACE} rollout status deployment/kube-autopsy-controller

echo "Reload complete"
