#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <image@sha256:digest> <output-dir> [--plain-http]" >&2
  exit 2
}

[ "$#" -eq 2 ] || [ "$#" -eq 3 ] || usage
image_ref="$1"
output_dir="$2"
plain_http="${3:-}"
[[ "$image_ref" =~ ^.+@sha256:[0-9a-f]{64}$ ]] || usage
if [ -n "$plain_http" ] && [ "$plain_http" != "--plain-http" ]; then
  usage
fi
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v oras >/dev/null || { echo "oras is required" >&2; exit 1; }
command -v sha256sum >/dev/null || { echo "sha256sum is required" >&2; exit 1; }

image_repo="${image_ref%@sha256:*}"
mkdir -p "$output_dir"
index_file="$output_dir/index.json"
oras_flags=()
if [ -n "$plain_http" ]; then
  oras_flags+=("$plain_http")
fi
oras manifest fetch "${oras_flags[@]}" --output "$index_file" "$image_ref"

jq -e '
  def runtime:
    .mediaType == "application/vnd.oci.image.manifest.v1+json" and
    (.digest | test("^sha256:[0-9a-f]{64}$")) and
    (.size | type == "number" and . > 0) and
    .platform.os == "linux" and
    (.platform.architecture == "amd64" or .platform.architecture == "arm64") and
    .annotations["vnd.docker.reference.type"] != "attestation-manifest";
  def attestation:
    .mediaType == "application/vnd.oci.image.manifest.v1+json" and
    (.digest | test("^sha256:[0-9a-f]{64}$")) and
    (.size | type == "number" and . > 0) and
    .annotations["vnd.docker.reference.type"] == "attestation-manifest" and
    (.annotations["vnd.docker.reference.digest"] | test("^sha256:[0-9a-f]{64}$"));
  .schemaVersion == 2 and
  .mediaType == "application/vnd.oci.image.index.v1+json" and
  ([.manifests[] | select(runtime)] | length == 2) and
  ([.manifests[] | select(runtime and .platform.architecture == "amd64")] | length == 1) and
  ([.manifests[] | select(runtime and .platform.architecture == "arm64")] | length == 1) and
  ([.manifests[] | select(attestation)] | length == 2) and
  all(.manifests[]; runtime or attestation) and
  ([.manifests[] | select(runtime) | .digest] | sort) ==
    ([.manifests[] | select(attestation) | .annotations["vnd.docker.reference.digest"]] | sort)
' "$index_file" >/dev/null

platform_rows="$output_dir/platforms.jsonl"
: > "$platform_rows"
for architecture in amd64 arm64; do
  manifest_digest="$(jq -er --arg arch "$architecture" '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest' "$index_file")"
  attestation_digest="$(jq -er --arg digest "$manifest_digest" '
    [.manifests[] | select(
      .annotations["vnd.docker.reference.type"] == "attestation-manifest" and
      .annotations["vnd.docker.reference.digest"] == $digest
    )] | if length == 1 then .[0].digest else error("expected one BuildKit attestation manifest") end
  ' "$index_file")"
  attestation_manifest="$output_dir/${architecture}-attestation-manifest.json"
  oras manifest fetch "${oras_flags[@]}" --output "$attestation_manifest" "${image_repo}@${attestation_digest}"
  jq -e --arg digest "$manifest_digest" '
    .schemaVersion == 2 and
    .artifactType == "application/vnd.docker.attestation.manifest.v1+json" and
    .subject.mediaType == "application/vnd.oci.image.manifest.v1+json" and
    .subject.digest == $digest and
    (.subject.size | type == "number" and . > 0) and
    .config.mediaType == "application/vnd.oci.empty.v1+json" and
    .config.digest == "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" and
    .config.size == 2 and
    (.layers | length == 2) and
    all(.layers[];
      .mediaType == "application/vnd.in-toto+json" and
      (.digest | test("^sha256:[0-9a-f]{64}$")) and
      (.size | type == "number" and . > 0) and
      (.annotations["in-toto.io/predicate-type"] == "https://slsa.dev/provenance/v1" or
       .annotations["in-toto.io/predicate-type"] == "https://spdx.dev/Document")
    ) and
    ([.layers[] | select(.mediaType == "application/vnd.in-toto+json" and .annotations["in-toto.io/predicate-type"] == "https://slsa.dev/provenance/v1")] | length == 1) and
    ([.layers[] | select(.mediaType == "application/vnd.in-toto+json" and .annotations["in-toto.io/predicate-type"] == "https://spdx.dev/Document")] | length == 1)
  ' "$attestation_manifest" >/dev/null

  provenance_blob="$(jq -er '.layers[] | select(.annotations["in-toto.io/predicate-type"] == "https://slsa.dev/provenance/v1") | .digest' "$attestation_manifest")"
  sbom_blob="$(jq -er '.layers[] | select(.annotations["in-toto.io/predicate-type"] == "https://spdx.dev/Document") | .digest' "$attestation_manifest")"
  provenance_statement="$output_dir/${architecture}-provenance.intoto.json"
  sbom_statement="$output_dir/${architecture}-sbom.intoto.json"
  oras blob fetch "${oras_flags[@]}" --output "$provenance_statement" "${image_repo}@${provenance_blob}"
  oras blob fetch "${oras_flags[@]}" --output "$sbom_statement" "${image_repo}@${sbom_blob}"

  manifest_hex="${manifest_digest#sha256:}"
  jq -e --arg digest "$manifest_hex" '
    ._type == "https://in-toto.io/Statement/v1" and
    .predicateType == "https://slsa.dev/provenance/v1" and
    (.subject | type == "array") and
    ((.subject | length) == 0 or
      ((.subject | length) == 1 and .subject[0].digest.sha256 == $digest)) and
    (.predicate | type == "object" and length > 0)
  ' "$provenance_statement" >/dev/null
  if [ -n "${QURL_EXPECTED_VCS_REVISION:-}" ]; then
    jq -e --arg revision "$QURL_EXPECTED_VCS_REVISION" '
      .predicate.buildDefinition.externalParameters.request.root.request.args["vcs:revision"] == $revision
    ' "$provenance_statement" >/dev/null
  fi
  if [ -n "${QURL_EXPECTED_VCS_SOURCE:-}" ]; then
    jq -e --arg source "$QURL_EXPECTED_VCS_SOURCE" '
      .predicate.buildDefinition.externalParameters.request.root.request.args["vcs:source"] == $source
    ' "$provenance_statement" >/dev/null
  fi
  jq -e --arg digest "$manifest_hex" '
    ._type == "https://in-toto.io/Statement/v1" and
    .predicateType == "https://spdx.dev/Document" and
    (.subject | type == "array") and
    ((.subject | length) == 0 or
      ((.subject | length) == 1 and .subject[0].digest.sha256 == $digest)) and
    .predicate.spdxVersion and
    .predicate.SPDXID == "SPDXRef-DOCUMENT"
  ' "$sbom_statement" >/dev/null

  provenance_statement_sha256="$(sha256sum "$provenance_statement" | awk '{print $1}')"
  sbom_statement_sha256="$(sha256sum "$sbom_statement" | awk '{print $1}')"
  [ "sha256:${provenance_statement_sha256}" = "$provenance_blob" ] || {
    echo "${architecture} provenance content does not match its OCI descriptor" >&2
    exit 1
  }
  [ "sha256:${sbom_statement_sha256}" = "$sbom_blob" ] || {
    echo "${architecture} SBOM content does not match its OCI descriptor" >&2
    exit 1
  }

  jq -nc \
    --arg platform "linux/${architecture}" \
    --arg manifest_digest "$manifest_digest" \
    --arg attestation_manifest_digest "$attestation_digest" \
    --arg provenance_blob_digest "$provenance_blob" \
    --arg provenance_statement_sha256 "$provenance_statement_sha256" \
    --arg sbom_blob_digest "$sbom_blob" \
    --arg sbom_statement_sha256 "$sbom_statement_sha256" \
    '{
      platform: $platform,
      manifest_digest: $manifest_digest,
      buildkit_attestation_manifest_digest: $attestation_manifest_digest,
      provenance: {blob_digest: $provenance_blob_digest, statement_sha256: $provenance_statement_sha256},
      sbom: {blob_digest: $sbom_blob_digest, statement_sha256: $sbom_statement_sha256}
    }' >> "$platform_rows"
done

jq -s --arg image "$image_ref" '{
  schema: "https://layerv.ai/attestations/qurl-image-buildkit-manifest/v1",
  image: $image,
  platforms: sort_by(.platform)
}' "$platform_rows" > "$output_dir/qurl-image-buildkit-manifest.json"
jq -e '.platforms | length == 2' "$output_dir/qurl-image-buildkit-manifest.json" >/dev/null
