#!/bin/bash -x

set -e

CLI_BIN="${CLI_BIN:=dist/stormforge_linux_amd64/stormforge}"

echo "Upload image to KinD"
[[ -n "${IMG}" ]] && kind load docker-image "${IMG}" --name chart-testing
[[ -n "${SETUPTOOLS_IMG}" ]] && kind load docker-image "${SETUPTOOLS_IMG}" --name chart-testing

echo "Initialize Controller"
${CLI_BIN} init

echo "Wait for controller"
${CLI_BIN} check controller --wait

echo "Create nginx deployment"
kubectl apply -f hack/nginx.yaml

stats() {
  ${CLI_BIN} generate experiment -f hack/app.yaml
  kubectl describe trials,jobs,pods -l stormforge.io/application=ci
  kubectl logs -n stormforge-system -l control-plane=controller-manager --tail=-1
}

generateAndWait() {
  echo "Create ci experiment"
  ${CLI_BIN} generate experiment -f "${1}" | \
    kubectl apply -f -

  echo "Create new trial"
  ${CLI_BIN} generate experiment -f "${1}" | \
  ${CLI_BIN} generate trial \
    --default base \
    -f - | \
    kubectl create -f -

  kubectl get pods -o wide
  kubectl get trial -o wide

  waitTime=300s
  echo "Wait for trial to complete (${waitTime} timeout)"
  kubectl wait trial \
    -l stormforge.io/application=ci \
    --for condition=stormforge.io/trial-complete \
    --timeout ${waitTime}

  echo "Wait for trial deletion tasks to finish running"
  kubectl wait deployment \
    optimize-default-prometheus-server \
    --for=delete \
    --timeout ${waitTime}
  kubectl wait job \
    -l stormforge.io/trial-role=trialSetup \
    --for condition=complete \
    --timeout ${waitTime}

  echo "Remove default experiment"
  ${CLI_BIN} generate experiment -f "${1}" | \
    kubectl delete -f -
}

trap stats EXIT

generateAndWait hack/app.yaml
generateAndWait hack/app_kube.yaml

