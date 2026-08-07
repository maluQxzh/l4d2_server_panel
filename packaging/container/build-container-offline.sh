#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

version="${VERSION:-dev}"
commit="${COMMIT:-$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || printf unknown)}"
build_date="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
platforms="${PLATFORMS:-linux/amd64,linux/arm64}"
output_dir="${OUTPUT_DIR:-$repo_root/dist/container}"
image_ref="l4d2panel/control:offline"
gzip_level="${GZIP_LEVEL:-6}"

if [[ ! "$version" =~ ^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$ ]]; then
    printf 'VERSION contains unsafe characters: %s\n' "$version" >&2
    exit 1
fi
if [[ ! "$gzip_level" =~ ^[1-9]$ ]]; then
    printf 'GZIP_LEVEL must be an integer from 1 through 9.\n' >&2
    exit 1
fi
for command_name in docker gzip sha256sum; do
    command -v "$command_name" >/dev/null 2>&1 || {
        printf 'Required command is unavailable: %s\n' "$command_name" >&2
        exit 1
    }
done
docker buildx inspect --bootstrap >/dev/null

mkdir -p "$output_dir"
cp "$repo_root/deploy/docker-compose.image.yml" "$output_dir/docker-compose.yml"
cp "$repo_root/.env.example" "$output_dir/l4d2panel-center.env.example"

build_args=(
    --build-arg "VERSION=$version"
    --build-arg "COMMIT=$commit"
    --build-arg "BUILD_DATE=$build_date"
)
if [[ -n "${GOPROXY:-}" ]]; then
    build_args+=(--build-arg "GOPROXY=$GOPROXY")
fi
if [[ -n "${ALPINE_MIRROR:-}" ]]; then
    build_args+=(--build-arg "ALPINE_MIRROR=$ALPINE_MIRROR")
fi

IFS=',' read -r -a requested_platforms <<<"$platforms"
for platform in "${requested_platforms[@]}"; do
    case "$platform" in
        linux/amd64) architecture=amd64 ;;
        linux/arm64) architecture=arm64 ;;
        *)
            printf 'Unsupported platform: %s\n' "$platform" >&2
            exit 1
            ;;
    esac

    printf 'Building %s for %s...\n' "$image_ref" "$platform"
    docker buildx build \
        --load \
        --platform "$platform" \
        --tag "$image_ref" \
        --provenance=false \
        --sbom=false \
        "${build_args[@]}" \
        "$repo_root"

    loaded_platform="$(docker image inspect "$image_ref" --format '{{.Os}}/{{.Architecture}}')"
    if [[ "$loaded_platform" != "$platform" ]]; then
        printf 'Loaded image platform is %s, expected %s.\n' "$loaded_platform" "$platform" >&2
        exit 1
    fi
    docker run --rm --platform "$platform" "$image_ref" --version

    artifact_name="l4d2panel-control-${version}-linux-${architecture}.tar.gz"
    artifact_path="$output_dir/$artifact_name"
    temporary_path="$artifact_path.tmp"
    rm -f "$temporary_path"
    if ! docker save "$image_ref" | gzip -n "-$gzip_level" >"$temporary_path"; then
        rm -f "$temporary_path"
        exit 1
    fi
    mv -f "$temporary_path" "$artifact_path"
    (
        cd "$output_dir"
        sha256sum "$artifact_name" >"$artifact_name.sha256"
    )
done

printf 'Container artifacts written to %s\n' "$output_dir"
