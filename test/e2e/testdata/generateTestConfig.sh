#!/bin/bash
# Karpenter Provider OCI
#
# Copyright (c) 2026 Oracle and/or its affiliates.
# Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/

set -euo pipefail

PREBAKED_IMAGE_COMPARTMENT_ID="ocid1.compartment.oc1..aaaaaaaab4u67dhgtj5gpdpp3z42xqqsdnufxkatoild46u3hb67vzojfmzq"
PREBAKED_IMAGE_COMPARTMENT_ID_UBUNTU="ocid1.compartment.oc1..aaaaaaaawapv5zqax243hxuvi5xs6ekpsntos2ylg2xyx6qnncctcab53hya"

TENANCY_ID="$1"
COMPARTMENT_NAME="$2"
IMAGE_TAG="$3"

# define constants
DRIFT_COMPARTMENT_NAME="${DRIFT_COMPARTMENT_NAME:-karpenter-e2e-drift}"
KEYS_COMPARTMENT_NAME="${KEYS_COMPARTMENT_NAME:-karpenter-e2e-keys}"
VAULT_NAME="${VAULT_NAME:-karpenter-e2e-vault}"
KMS_KEY1_NAME="${KMS_KEY1_NAME:-karpenter-key-1}"
KMS_KEY2_NAME="${KMS_KEY2_NAME:-karpenter-key-2}"
NODE_SUBNET1_NAME="${NODE_SUBNET1_NAME:-private-worker-subnet}"
NODE_SUBNET2_NAME="${NODE_SUBNET2_NAME:-public-worker-drift-subnet}"
NSG1_NAME="${NSG1_NAME:-karpenter-network-security-group}"
NSG2_NAME="${NSG2_NAME:-karpenter-network-security-group-2}"
CAPACITY_RESERVATION1_NAME="${CAPACITY_RESERVATION1_NAME:-karpenter-cap-res-1}"
CAPACITY_RESERVATION2_NAME="${CAPACITY_RESERVATION2_NAME:-karpenter-cap-res-2}"
COMPUTE_CLUSTER_NAME="${COMPUTE_CLUSTER_NAME:-karpenter-e2e-compute-cluster}"
NPN_CLUSTER_NAME="${NPN_CLUSTER_NAME:-karpenter-npn-cluster}"
FLANNEL_CLUSTER_NAME="${FLANNEL_CLUSTER_NAME:-karpenter-flannel-cluster}"
UBUNTU_IMAGE_NAME="${UBUNTU_IMAGE_NAME:-ubuntu-amd64-minimal-24.04-noble-v20250604.1-OKE-1.32.1}"
CUSTOM_IMAGE_NAME="${CUSTOM_IMAGE_NAME:-custom-image-karpenter-testing}"
IMAGE_REGISTRY="${IMAGE_REGISTRY:-ghcr.io}"
IMAGE_REPOSITORY_NAME="${IMAGE_REPOSITORY_NAME:-oracle/karpenter-provider-oci}"
TEST_DEPLOYMENT_IMAGE="${TEST_DEPLOYMENT_IMAGE:-docker.io/library/busybox:latest}"
if [[ -z "${SSH_PUB_KEY:-}" ]]; then
  for key_file in "$HOME/.ssh/id_rsa.pub" "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_ecdsa.pub"; do
    if [[ -f "$key_file" ]]; then
      SSH_PUB_KEY="$(tr -d "\n" < "$key_file")"
      break
    fi
  done
fi
: "${SSH_PUB_KEY:?set SSH_PUB_KEY or ensure a public key exists in ~/.ssh}"

# configure oci cli
export OCI_CLI_DEBUG="${OCI_CLI_DEBUG:-false}"
# Default to instance_principal, but allow override from environment
export OCI_CLI_AUTH="${OCI_CLI_AUTH:-instance_principal}"

# auth method for tests (default to PROFILE_SESSION; can be overridden via env)
OCI_AUTH_METHOD_FOR_TEST="${OCI_AUTH_METHOD_FOR_TEST:-PROFILE_SESSION}"

# start doing lookups
CE_ENDPOINT_ARGS=()
if [[ -n "${ENDPOINT:-}" ]]; then
  CE_ENDPOINT_ARGS=(--endpoint "$ENDPOINT")
fi

export COMPARTMENT_ID=$(oci iam compartment list --all --compartment-id "$TENANCY_ID" --name "$COMPARTMENT_NAME" --query 'data[0].id' --raw-output)
DRIFT_COMPARTMENT_ID=$(oci iam compartment list --all --compartment-id "$COMPARTMENT_ID" --name "$DRIFT_COMPARTMENT_NAME" --query 'data[0].id' --raw-output)
KEYS_COMPARTMENT_ID=$(oci iam compartment list --all --compartment-id "$TENANCY_ID" --name "$KEYS_COMPARTMENT_NAME" --query 'data[0].id' --raw-output)
VCN_ID=$(oci network vcn list --compartment-id "$COMPARTMENT_ID" --display-name karpenter_vcn --query 'data[0].id' --raw-output)
NODE_SUBNET1_ID=$(oci network subnet list --compartment-id "$COMPARTMENT_ID" --vcn-id "$VCN_ID" --display-name "$NODE_SUBNET1_NAME" --query 'data[0].id' --raw-output)
NODE_SUBNET2_ID=$(oci network subnet list --compartment-id "$COMPARTMENT_ID" --vcn-id "$VCN_ID" --display-name "$NODE_SUBNET2_NAME" --query 'data[0].id' --raw-output)
VAULT_ID=$(oci kms management vault list --compartment-id "$KEYS_COMPARTMENT_ID" --query "data[?\"display-name\"=='$VAULT_NAME'].id | [0]" --raw-output)
KMS_ENDPOINT=$(oci kms management vault get --vault-id "$VAULT_ID" --query 'data."management-endpoint"' --raw-output)
KMS_KEY1_ID=$(oci kms management key list --endpoint "$KMS_ENDPOINT" --all --compartment-id "$COMPARTMENT_ID" --query "data[?\"display-name\"=='$KMS_KEY1_NAME' && \"lifecycle-state\"=='ENABLED'].id | [0]" --raw-output)
KMS_KEY2_ID=$(oci kms management key list --endpoint "$KMS_ENDPOINT" --all --compartment-id "$COMPARTMENT_ID" --query "data[?\"display-name\"=='$KMS_KEY2_NAME' && \"lifecycle-state\"=='ENABLED'].id | [0]" --raw-output)
NSG1_ID=$(oci network nsg list --compartment-id "$COMPARTMENT_ID" --vcn-id "$VCN_ID" --display-name "$NSG1_NAME" --query 'data[0].id' --raw-output)
NSG2_ID=$(oci network nsg list --compartment-id "$COMPARTMENT_ID" --vcn-id "$VCN_ID" --display-name "$NSG2_NAME" --query 'data[0].id' --raw-output)
CAPACITY_RESERVATION1_ID=$(oci compute capacity-reservation list --compartment-id "$COMPARTMENT_ID" --query "data[?\"display-name\"=='$CAPACITY_RESERVATION1_NAME'].id | [0]" --raw-output)
CAPACITY_RESERVATION2_ID=$(oci compute capacity-reservation list --compartment-id "$COMPARTMENT_ID" --query "data[?\"display-name\"=='$CAPACITY_RESERVATION2_NAME'].id | [0]" --raw-output)
COMPUTE_CLUSTER_ID=$(oci compute compute-cluster list --compartment-id "$COMPARTMENT_ID" --query "data.items[?\"display-name\"=='$COMPUTE_CLUSTER_NAME'].id | [0]" --raw-output)
export NPN_CLUSTER_ID=$(oci ce cluster list "${CE_ENDPOINT_ARGS[@]}" --compartment-id "$COMPARTMENT_ID" --lifecycle-state ACTIVE --name "$NPN_CLUSTER_NAME" --query 'data[0].id' --raw-output)
NPN_KUBEAPI_ENDPOINT_IP=$(oci ce cluster get "${CE_ENDPOINT_ARGS[@]}" --cluster-id "$NPN_CLUSTER_ID" --query 'data.endpoints' --raw-output | jq -r '.["private-endpoint"] | split(":")[0]' )
export FLANNEL_CLUSTER_ID=$(oci ce cluster list "${CE_ENDPOINT_ARGS[@]}" --compartment-id "$COMPARTMENT_ID" --lifecycle-state ACTIVE --name "$FLANNEL_CLUSTER_NAME" --query 'data[0].id' --raw-output)
FLANNEL_KUBEAPI_ENDPOINT_IP=$(oci ce cluster get "${CE_ENDPOINT_ARGS[@]}" --cluster-id "$FLANNEL_CLUSTER_ID" --query 'data.endpoints' --raw-output | jq -r '.["private-endpoint"] | split(":")[0]')

# print out the variables
echo "TENANCY_ID: $TENANCY_ID"
echo "COMPARTMENT_NAME: $COMPARTMENT_NAME"
echo "IMAGE_TAG: $IMAGE_TAG"
echo "COMPARTMENT_ID: $COMPARTMENT_ID"
echo "DRIFT_COMPARTMENT_ID: $DRIFT_COMPARTMENT_ID"
echo "KEYS_COMPARTMENT_ID: $KEYS_COMPARTMENT_ID"
echo "NPN_CLUSTER_ID: $NPN_CLUSTER_ID"
echo "NPN_KUBEAPI_ENDPOINT_IP: $NPN_KUBEAPI_ENDPOINT_IP"
echo "FLANNEL_CLUSTER_ID: $FLANNEL_CLUSTER_ID"
echo "FLANNEL_KUBEAPI_ENDPOINT_IP: $FLANNEL_KUBEAPI_ENDPOINT_IP"
echo "VCN_ID: $VCN_ID"
echo "NODE_SUBNET1_ID: $NODE_SUBNET1_ID"
echo "NODE_SUBNET2_ID: $NODE_SUBNET2_ID"
echo "VAULT_ID: $VAULT_ID"
echo "KMS_ENDPOINT: $KMS_ENDPOINT"
echo "KMS_KEY1_ID: $KMS_KEY1_ID"
echo "KMS_KEY2_ID: $KMS_KEY2_ID"
echo "NSG1_ID: $NSG1_ID"
echo "NSG2_ID: $NSG2_ID"
echo "CAPACITY_RESERVATION1_ID: $CAPACITY_RESERVATION1_ID"
echo "CAPACITY_RESERVATION2_ID: $CAPACITY_RESERVATION2_ID"
echo "COMPUTE_CLUSTER_ID: $COMPUTE_CLUSTER_ID"

# Derive image IDs per provider filterAndSortImages logic and set display name placeholders
# Template and output file paths
TEMPLATE_FLANNEL_JSON="e2e_test_config_flannel.template"
TEMPLATE_NPN_JSON="e2e_test_config_npn.template"
TEMPLATE_FLANNEL_VALUES="e2e_test_helm_values_flannel.template"
TEMPLATE_NPN_VALUES="e2e_test_helm_values_npn.template"

# Output generated files
OUT_FLANNEL_JSON="e2e_test_config_flannel.json"
OUT_NPN_JSON="e2e_test_config_npn.json"
OUT_FLANNEL_VALUES="e2e_test_helm_values_flannel.yaml"
OUT_NPN_VALUES="e2e_test_helm_values_npn.yaml"

# Read OS filters and target cluster k8s version (from FLANNEL cluster) using the flannel config template
IMAGE_OS=$(jq -r '.OCINodeClass.ImageOsFilter // empty' "$TEMPLATE_FLANNEL_JSON")
IMAGE_OS_VERSION=$(jq -r '.OCINodeClass.ImageOsVersionFilter // empty' "$TEMPLATE_FLANNEL_JSON")
TARGET_K8S_VERSION=$(oci ce cluster get "${CE_ENDPOINT_ARGS[@]}" --cluster-id "$FLANNEL_CLUSTER_ID" --query 'data."kubernetes-version"' --raw-output)

# Parse major/minor from version (strip leading v)
VER_TRIMMED="${TARGET_K8S_VERSION#v}"
MAJOR="${VER_TRIMMED%%.*}"
REST="${VER_TRIMMED#*.}"
MINOR="${REST%%.*}"

# Build compute list args against pre-baked image compartment (like provider)
COMPUTE_LIST_ARGS=(--compartment-id "$PREBAKED_IMAGE_COMPARTMENT_ID" --all)
if [[ -n "$IMAGE_OS" ]]; then COMPUTE_LIST_ARGS+=(--operating-system "$IMAGE_OS"); fi
if [[ -n "$IMAGE_OS_VERSION" ]]; then COMPUTE_LIST_ARGS+=(--operating-system-version "$IMAGE_OS_VERSION"); fi

IMAGE_ID=""
IMAGE_DISPLAY_NAME=""
DRIFT_IMAGE_ID=""
DRIFT_IMAGE_DISPLAY_NAME=""

# New template variables: Ubuntu/custom image ID lookup by display name
UBUNTU_IMAGE_ID=""
CUSTOM_IMAGE_ID=""

# Build candidate list sorted by:
#  1) minimal version skew score (as in kubeletVersionCompatibleScore)
#  2) newest time-created first (stable tie-breaking like provider's pre-sort)
if [[ -n "$MAJOR" && -n "$MINOR" ]]; then
  CANDIDATES_JSON=$(
    oci compute image list "${COMPUTE_LIST_ARGS[@]}" --query 'data' |
    jq -r --arg maj "$MAJOR" --arg min "$MINOR" '
      # Exclude ARM/aarch64 images; e2e currently targets amd64/x86_64 only
      map(select(."display-name" | test("aarch64"; "i") | not)) |
      map(select(."freeform-tags".k8s_version != null)) |
      map(
        . as $img |
        (."freeform-tags".k8s_version | ascii_downcase | ltrimstr("v") | split(".") | {maj: (.[0]|tonumber), min: (.[1]|tonumber)}) as $iv |
        (($iv.maj != ($maj|tonumber)) or ($iv.min > ($min|tonumber))) as $incompatibleHigher |
        ((($min|tonumber) - $iv.min)) as $diff |
        ((($iv.maj==1) and ($iv.min < 25) and ($diff > 2)) or ($diff > 3)) as $exceedsSkew |
        ($img."time-created"
          | try (
              capture("^(?<d>\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2})(?:\\.\\d+)?(?:(?<z>Z)|(?<sign>[+-])(?<hh>\\d{2}):(?<mm>\\d{2}))$")
              | "\(.d)\(if .z then "+0000" else "\(.sign)\(.hh)\(.mm)" end)"
              | strptime("%Y-%m-%dT%H:%M:%S%z")
              | mktime
            ) catch 0
          ) as $tc |
        . + {
          score: (if $incompatibleHigher or $exceedsSkew then -1 else $diff end),
          tc: $tc
        }
      ) |
      map(select(.score >= 0))
    '
  )

  # Primary = best candidate (min score, newest time-created on tie)
  PRIMARY_MIN_SCORE=$(printf '%s' "$CANDIDATES_JSON" | jq -r 'if length>0 then (min_by(.score).score) else empty end')
  if [[ -n "$PRIMARY_MIN_SCORE" ]]; then
    IMAGE_ID=$(printf '%s' "$CANDIDATES_JSON" | jq -r --argjson min "$PRIMARY_MIN_SCORE" '([ .[] | select(.score == $min) ] | max_by(.tc)).id')
    IMAGE_DISPLAY_NAME=$(printf '%s' "$CANDIDATES_JSON" | jq -r --argjson min "$PRIMARY_MIN_SCORE" '([ .[] | select(.score == $min) ] | max_by(.tc))."display-name"')
    IMAGE_DISPLAY_NAME=${IMAGE_DISPLAY_NAME%%$'\t'*}
    PRIMARY_SCORE="$PRIMARY_MIN_SCORE"
  fi

  # Drift selection: choose next compatible minimal score greater than primary (e.g., previous minor), newest on tie
  if [[ -n "$IMAGE_ID" ]]; then
    DRIFT_MIN_SCORE=$(printf '%s' "$CANDIDATES_JSON" | jq -r --arg primary "$IMAGE_ID" --argjson ps "${PRIMARY_SCORE:-0}" '
      [ .[] | select(.score > $ps and .id != $primary) ] as $rest |
      if ($rest|length)>0 then ($rest | min_by(.score).score) else empty end')
    if [[ -n "$DRIFT_MIN_SCORE" ]]; then
      DRIFT_IMAGE_ID=$(printf '%s' "$CANDIDATES_JSON" | jq -r --arg primary "$IMAGE_ID" --argjson ds "$DRIFT_MIN_SCORE" '
        [ .[] | select(.score == $ds and .id != $primary) ] | max_by(.tc).id')
      DRIFT_IMAGE_DISPLAY_NAME=$(printf '%s' "$CANDIDATES_JSON" | jq -r --arg primary "$IMAGE_ID" --argjson ds "$DRIFT_MIN_SCORE" '
        [ .[] | select(.score == $ds and .id != $primary) ] | max_by(.tc)."display-name"')
      DRIFT_IMAGE_DISPLAY_NAME=${DRIFT_IMAGE_DISPLAY_NAME%%$'\t'*}
    fi
  fi
fi

# Resolve Ubuntu/custom image IDs by image display-name
# - Ubuntu images live in PREBAKED_IMAGE_COMPARTMENT_ID_UBUNTU
# - Custom image lives in COMPARTMENT_ID (test compartment)
if [[ -n "${UBUNTU_IMAGE_NAME:-}" ]]; then
  UBUNTU_IMAGE_ID=$(oci compute image list --all --compartment-id "$PREBAKED_IMAGE_COMPARTMENT_ID_UBUNTU" \
    --query "data[?\"display-name\"=='${UBUNTU_IMAGE_NAME}'].id | [0]" --raw-output)
fi

if [[ -n "${CUSTOM_IMAGE_NAME:-}" ]]; then
  CUSTOM_IMAGE_ID=$(oci compute image list --all --compartment-id "$COMPARTMENT_ID" \
    --query "data[?\"display-name\"=='${CUSTOM_IMAGE_NAME}'].id | [0]" --raw-output)
fi

# Print resolved values
echo "IMAGE_ID: ${IMAGE_ID:-}"
echo "IMAGE_DISPLAY_NAME: ${IMAGE_DISPLAY_NAME:-}"
echo "DRIFT_IMAGE_ID: ${DRIFT_IMAGE_ID:-}"
echo "DRIFT_IMAGE_DISPLAY_NAME: ${DRIFT_IMAGE_DISPLAY_NAME:-}"
echo "UBUNTU_IMAGE_ID: ${UBUNTU_IMAGE_ID:-}"
echo "CUSTOM_IMAGE_ID: ${CUSTOM_IMAGE_ID:-}"

# Prepare generated files from templates
cp "$TEMPLATE_FLANNEL_JSON" "$OUT_FLANNEL_JSON"
cp "$TEMPLATE_NPN_JSON" "$OUT_NPN_JSON"
cp "$TEMPLATE_FLANNEL_VALUES" "$OUT_FLANNEL_VALUES"
cp "$TEMPLATE_NPN_VALUES" "$OUT_NPN_VALUES"

# Replace place-holders with the lookup values in generated files
FILES=("$OUT_FLANNEL_JSON" "$OUT_NPN_JSON" "$OUT_FLANNEL_VALUES" "$OUT_NPN_VALUES")

VARIABLES=(
  "COMPARTMENT_NAME" "DRIFT_COMPARTMENT_NAME" "COMPARTMENT_ID" "DRIFT_COMPARTMENT_ID"
  "KMS_KEY1_NAME" "KMS_KEY2_NAME" "KMS_KEY1_ID" "KMS_KEY2_ID"
  "NODE_SUBNET1_NAME" "NODE_SUBNET2_NAME" "NODE_SUBNET1_ID" "NODE_SUBNET2_ID"
  "NSG1_NAME" "NSG2_NAME" "NSG1_ID" "NSG2_ID"
  "CAPACITY_RESERVATION1_NAME" "CAPACITY_RESERVATION2_NAME" "CAPACITY_RESERVATION1_ID" "CAPACITY_RESERVATION2_ID"
  "COMPUTE_CLUSTER_NAME" "COMPUTE_CLUSTER_ID"
  "NPN_KUBEAPI_ENDPOINT_IP" "FLANNEL_KUBEAPI_ENDPOINT_IP"
  "IMAGE_ID" "IMAGE_DISPLAY_NAME" "DRIFT_IMAGE_ID" "DRIFT_IMAGE_DISPLAY_NAME" "OCI_AUTH_METHOD_FOR_TEST" "IMAGE_TAG"
  "IMAGE_REGISTRY" "IMAGE_REPOSITORY_NAME" "TEST_DEPLOYMENT_IMAGE" "SSH_PUB_KEY"
  "UBUNTU_IMAGE_ID" "CUSTOM_IMAGE_ID"
)

sedit() {
  # GNU sed supports '-i' without arg; BSD/macOS sed requires a backup suffix ('' for none)
  if sed --version >/dev/null 2>&1; then
    sed -i "$@"
  else
    sed -i '' "$@"
  fi
}

for file in "${FILES[@]}"; do
  for VAR in "${VARIABLES[@]}"; do
    val="${!VAR:-}"
    [ -n "$val" ] || continue
    sedit "s/VAR_${VAR}/$(printf '%s' "$val" | sed -e 's/[\/&]/\\&/g')/g" "$file"
  done
done
