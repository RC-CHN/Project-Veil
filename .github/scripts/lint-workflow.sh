#!/usr/bin/env bash
set -euo pipefail

# A small, checksum-pinned binary avoids compiling Go before jobs can fan out.
folder=$(mktemp -d)
trap 'rm -rf "$folder"' EXIT
curl --fail --silent --show-error --location --retry 2 \
  https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz \
  --output "$folder/actionlint.tar.gz"
printf '%s  %s\n' \
  8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8 \
  "$folder/actionlint.tar.gz" | sha256sum --check --status
tar -xzf "$folder/actionlint.tar.gz" -C "$folder" actionlint
"$folder/actionlint" -color
