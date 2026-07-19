#!/usr/bin/env bash
# =============================================================================
# kube-autopsy end-to-end test using kind
# =============================================================================
#
# This script:
#   1. Creates a kind cluster
#   2. Builds the kube-autopsy Docker image and loads it into kind
#   3. Deploys the CRD, RBAC, controller, and DaemonSet agent
#   4. Deploys test pods that trigger OOM kills
#   5. Verifies that PodCrashReport CRDs are created with correct data
#   6. Cleans up
#
# Usage:
#   ./test/e2e/run.sh              # Full run (create cluster, test, cleanup)
#   ./test/e2e/run.sh --no-cleanup # Keep cluster after test for debugging
#   CLUSTER_NAME=my-test ./test/e2e/run.sh  # Custom cluster name
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-kube-autopsy-e2e}"
IMAGE_NAME="kube-autopsy:e2e"
KIND_CONFIG="${SCRIPT_DIR}/kind-config.yaml"
TEST_PODS="${SCRIPT_DIR}/testdata/test-pods.yaml"
TIMEOUT_REPORT="${TIMEOUT_REPORT:-120}"    # seconds to wait for crash reports
TIMEOUT_POD="${TIMEOUT_POD:-60}"           # seconds to wait for pods to start
CLEANUP="${1:-}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# Counters
TESTS_PASSED=0
TESTS_FAILED=0
TESTS_TOTAL=0

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log()  { echo -e "${CYAN}[$(date +%H:%M:%S)]${NC} $*"; }
ok()   { echo -e "${GREEN}  ✔ $*${NC}"; }
fail() { echo -e "${RED}  ✘ $*${NC}"; }
warn() { echo -e "${YELLOW}  ⚠ $*${NC}"; }
header() { echo -e "\n${BOLD}━━━ $* ━━━${NC}"; }

assert() {
    local desc="$1"
    shift
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
    if "$@" >/dev/null 2>&1; then
        ok "PASS: ${desc}"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        return 0
    else
        fail "FAIL: ${desc}"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    fi
}

assert_contains() {
    local desc="$1"
    local haystack="$2"
    local needle="$3"
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
    if echo "$haystack" | grep -q "$needle"; then
        ok "PASS: ${desc}"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        return 0
    else
        fail "FAIL: ${desc} (expected to contain '${needle}')"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    fi
}

wait_for_condition() {
    local desc="$1"
    local timeout="$2"
    shift 2
    local start end elapsed
    start=$(date +%s)
    end=$((start + timeout))

    while true; do
        if "$@" >/dev/null 2>&1; then
            elapsed=$(( $(date +%s) - start ))
            log "${desc} (took ${elapsed}s)"
            return 0
        fi
        if [ "$(date +%s)" -ge "$end" ]; then
            fail "Timeout (${timeout}s): ${desc}"
            return 1
        fi
        sleep 2
    done
}

# ---------------------------------------------------------------------------
# Cleanup trap
# ---------------------------------------------------------------------------
cleanup() {
    if [ "$CLEANUP" = "--no-cleanup" ]; then
        warn "Skipping cleanup (--no-cleanup). Cluster '${CLUSTER_NAME}' is still running."
        warn "Delete manually: kind delete cluster --name ${CLUSTER_NAME}"
        return
    fi

    header "Cleanup"
    log "Deleting kind cluster '${CLUSTER_NAME}'..."
    kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
    ok "Cluster deleted"
}

# ---------------------------------------------------------------------------
# Phase 1: Create kind cluster
# ---------------------------------------------------------------------------
phase_create_cluster() {
    header "Phase 1: Create kind cluster"

    # Delete existing cluster if it exists
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log "Deleting existing cluster '${CLUSTER_NAME}'..."
        kind delete cluster --name "${CLUSTER_NAME}"
    fi

    log "Creating kind cluster '${CLUSTER_NAME}'..."
    kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 60s
    ok "Cluster created"

    log "Waiting for node to be Ready..."
    kubectl wait --for=condition=Ready node --all --timeout=60s
    ok "Node ready"
}

# ---------------------------------------------------------------------------
# Phase 2: Build and load image
# ---------------------------------------------------------------------------
phase_build_image() {
    header "Phase 2: Build and load Docker image"

    log "Building image '${IMAGE_NAME}'..."
    docker build -t "${IMAGE_NAME}" "${PROJECT_ROOT}"
    ok "Image built"

    log "Loading image into kind cluster..."
    kind load docker-image "${IMAGE_NAME}" --name "${CLUSTER_NAME}"
    ok "Image loaded"
}

# ---------------------------------------------------------------------------
# Phase 3: Deploy kube-autopsy
# ---------------------------------------------------------------------------
phase_deploy() {
    header "Phase 3: Deploy kube-autopsy"

    log "Applying CRD..."
    kubectl apply -f "${PROJECT_ROOT}/deploy/base/crd.yaml"
    ok "CRD applied"

    log "Applying namespace, RBAC, and service account..."
    kubectl apply -f "${PROJECT_ROOT}/deploy/base/namespace.yaml"
    kubectl apply -f "${PROJECT_ROOT}/deploy/base/serviceaccount.yaml"
    kubectl apply -f "${PROJECT_ROOT}/deploy/base/clusterrole.yaml"
    kubectl apply -f "${PROJECT_ROOT}/deploy/base/clusterrolebinding.yaml"
    ok "RBAC applied"

    # Patch manifests to use the e2e image (instead of ghcr.io/stone/kube-autopsy:latest)
    log "Deploying controller..."
    sed -E "s|ghcr.io/stone/kube-autopsy:latest|${IMAGE_NAME}|g; s|kube-autopsy:latest|${IMAGE_NAME}|g" "${PROJECT_ROOT}/deploy/base/deployment.yaml" \
        | sed 's/--leader-elect=true/--leader-elect=false/' \
        | kubectl apply -f -
    ok "Controller deployed"

    log "Deploying DaemonSet agent..."
    sed -E "s|ghcr.io/stone/kube-autopsy:latest|${IMAGE_NAME}|g; s|kube-autopsy:latest|${IMAGE_NAME}|g" "${PROJECT_ROOT}/deploy/base/daemonset.yaml" \
        | kubectl apply -f -
    ok "DaemonSet applied"

    log "Waiting for controller to be ready..."
    wait_for_condition "Controller ready" 60 \
        kubectl -n kube-autopsy rollout status deployment/kube-autopsy-controller --timeout=5s

    log "Waiting for DaemonSet agent to be ready..."
    wait_for_condition "Agent ready" 60 \
        kubectl -n kube-autopsy rollout status daemonset/kube-autopsy-agent --timeout=5s

    ok "All kube-autopsy components running"
    echo
    kubectl -n kube-autopsy get pods
}

# ---------------------------------------------------------------------------
# Phase 4: Run test workloads
# ---------------------------------------------------------------------------
phase_run_tests() {
    header "Phase 4: Run test workloads"

    log "Deploying test pods..."
    kubectl apply -f "${TEST_PODS}"
    ok "Test pods deployed"

    # Wait for the OOM victim to be terminated
    log "Waiting for oom-victim pod to be OOM killed..."
    wait_for_condition "oom-victim terminated" "${TIMEOUT_POD}" \
        bash -c "kubectl get pod oom-victim -o jsonpath='{.status.containerStatuses[0].state.terminated.reason}' 2>/dev/null | grep -q 'OOMKilled'"
    ok "oom-victim was OOM killed"

    # Wait for multi-container victim
    log "Waiting for multi-container-victim main-app to be OOM killed..."
    wait_for_condition "multi-container-victim terminated" "${TIMEOUT_POD}" \
        bash -c "kubectl get pod multi-container-victim -o jsonpath='{.status.containerStatuses[?(@.name==\"main-app\")].state.terminated.reason}' 2>/dev/null | grep -q 'OOMKilled'"
    ok "multi-container-victim/main-app was OOM killed"

    echo
    log "Pod statuses:"
    kubectl get pods -l test=kube-autopsy-e2e -o wide
}

# ---------------------------------------------------------------------------
# Phase 5: Verify PodCrashReports
# ---------------------------------------------------------------------------
phase_verify_reports() {
    header "Phase 5: Verify PodCrashReports"

    # Wait for at least one PodCrashReport to appear
    log "Waiting for PodCrashReport CRDs to be created..."
    wait_for_condition "At least one PodCrashReport exists" "${TIMEOUT_REPORT}" \
        bash -c "[ \$(kubectl get podcrashreports --all-namespaces --no-headers 2>/dev/null | wc -l) -ge 1 ]"

    # Give the system a few more seconds to create all reports
    sleep 10

    echo
    log "All PodCrashReports:"
    kubectl get podcrashreports --all-namespaces -o wide 2>/dev/null || true
    echo

    # -----------------------------------------------------------------------
    # Test 1: OOM victim report exists
    # -----------------------------------------------------------------------
    header "Test 1: OOM victim PodCrashReport"

    local oom_report
    oom_report=$(kubectl get podcrashreports --all-namespaces -o json 2>/dev/null \
        | jq -r '.items[] | select(.spec.podName == "oom-victim")' 2>/dev/null)

    assert "PodCrashReport exists for oom-victim" \
        test -n "$oom_report"

    if [ -n "$oom_report" ]; then
        local oom_reason oom_exit oom_container oom_phase oom_log_count oom_peak

        oom_reason=$(echo "$oom_report" | jq -r '.spec.terminationReason')
        oom_exit=$(echo "$oom_report" | jq -r '.spec.exitCode')
        oom_container=$(echo "$oom_report" | jq -r '.spec.containerName')
        oom_phase=$(echo "$oom_report" | jq -r '.status.phase // empty')
        oom_log_count=$(echo "$oom_report" | jq -r '.status.diagnostics.lastLogLines | length')
        oom_peak=$(echo "$oom_report" | jq -r '.status.diagnostics.peakMemoryBytes')

        assert_contains "terminationReason is OOMKilled" "$oom_reason" "OOMKilled"
        assert "exitCode is 137" test "$oom_exit" = "137"
        assert "containerName is 'hogger'" test "$oom_container" = "hogger"
        assert "status.phase is set" test -n "$oom_phase"
        assert "lastLogLines captured (count > 0)" test "$oom_log_count" -gt 0
        assert "peakMemoryBytes captured (> 0)" test "$oom_peak" -gt 0

        echo
        log "OOM victim report (YAML):"
        kubectl get podcrashreports --all-namespaces -o json 2>/dev/null \
            | jq '.items[] | select(.spec.podName == "oom-victim")' || true
    fi

    # -----------------------------------------------------------------------
    # Test 2: Multi-container victim report
    # -----------------------------------------------------------------------
    header "Test 2: Multi-container PodCrashReport"

    local multi_report
    multi_report=$(kubectl get podcrashreports --all-namespaces -o json 2>/dev/null \
        | jq -r '.items[] | select(.spec.podName == "multi-container-victim")' 2>/dev/null)

    assert "PodCrashReport exists for multi-container-victim" \
        test -n "$multi_report"

    if [ -n "$multi_report" ]; then
        local multi_container
        multi_container=$(echo "$multi_report" | jq -r '.spec.containerName')
        assert "containerName is 'main-app' (not sidecar)" test "$multi_container" = "main-app"
    fi

    # -----------------------------------------------------------------------
    # Test 3: Report listing and kubectl shortname
    # -----------------------------------------------------------------------
    header "Test 3: kubectl integration"

    assert "kubectl get pcr works (shortName)" \
        kubectl get pcr --all-namespaces

    local pcr_output
    pcr_output=$(kubectl get pcr --all-namespaces 2>/dev/null)

    assert_contains "pcr output shows Pod column" "$pcr_output" "POD"
    assert_contains "pcr output shows Reason column" "$pcr_output" "REASON"
    assert_contains "pcr output shows Node column" "$pcr_output" "NODE"

    # -----------------------------------------------------------------------
    # Test 4: Controller health check
    # -----------------------------------------------------------------------
    header "Test 4: Controller health"

    log "Checking controller health endpoints..."
    local controller_pod
    controller_pod=$(kubectl -n kube-autopsy get pods -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [ -n "$controller_pod" ]; then
        assert "Controller pod is Running" \
            bash -c "kubectl -n kube-autopsy get pod ${controller_pod} -o jsonpath='{.status.phase}' | grep -q 'Running'"

        # Check that there are no restarts
        local restarts
        restarts=$(kubectl -n kube-autopsy get pod "${controller_pod}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)
        assert "Controller has 0 restarts" test "$restarts" = "0"
    fi

    # -----------------------------------------------------------------------
    # Test 5: Agent health check
    # -----------------------------------------------------------------------
    header "Test 5: Agent health"

    local agent_pod
    agent_pod=$(kubectl -n kube-autopsy get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [ -n "$agent_pod" ]; then
        assert "Agent pod is Running" \
            bash -c "kubectl -n kube-autopsy get pod ${agent_pod} -o jsonpath='{.status.phase}' | grep -q 'Running'"

        local agent_restarts
        agent_restarts=$(kubectl -n kube-autopsy get pod "${agent_pod}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)
        assert "Agent has 0 restarts" test "$agent_restarts" = "0"

        echo
        log "Agent logs (last 20 lines):"
        kubectl -n kube-autopsy logs "${agent_pod}" --tail=20 2>/dev/null || true
    fi
}

# ---------------------------------------------------------------------------
# Phase 6: Collect debug info on failure
# ---------------------------------------------------------------------------
collect_debug_info() {
    header "Debug Information"

    log "kube-autopsy pods:"
    kubectl -n kube-autopsy get pods -o wide 2>/dev/null || true
    echo

    log "kube-autopsy controller logs:"
    kubectl -n kube-autopsy logs -l app.kubernetes.io/component=controller --tail=50 2>/dev/null || true
    echo

    log "kube-autopsy agent logs:"
    kubectl -n kube-autopsy logs -l app.kubernetes.io/component=agent --tail=50 2>/dev/null || true
    echo

    log "All PodCrashReports (full YAML):"
    kubectl get podcrashreports --all-namespaces -o yaml 2>/dev/null || true
    echo

    log "Test pods:"
    kubectl get pods -l test=kube-autopsy-e2e -o wide 2>/dev/null || true
    echo

    log "Events in default namespace:"
    kubectl get events --sort-by='.lastTimestamp' 2>/dev/null | tail -20 || true
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
    header "Test Summary"

    echo
    echo -e "  Total:  ${BOLD}${TESTS_TOTAL}${NC}"
    echo -e "  Passed: ${GREEN}${TESTS_PASSED}${NC}"
    echo -e "  Failed: ${RED}${TESTS_FAILED}${NC}"
    echo

    if [ "$TESTS_FAILED" -gt 0 ]; then
        echo -e "${RED}${BOLD}  SOME TESTS FAILED${NC}"
        return 1
    else
        echo -e "${GREEN}${BOLD}  ALL TESTS PASSED ✔${NC}"
        return 0
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    header "kube-autopsy e2e test"
    log "Cluster: ${CLUSTER_NAME}"
    log "Image:   ${IMAGE_NAME}"
    log "Project: ${PROJECT_ROOT}"
    echo

    trap cleanup EXIT

    phase_create_cluster
    phase_build_image
    phase_deploy
    phase_run_tests
    phase_verify_reports

    if [ "$TESTS_FAILED" -gt 0 ]; then
        collect_debug_info
    fi

    print_summary
}

main "$@"
