#!/usr/bin/env bash
set -euo pipefail
buf lint
buf generate
echo "Proto codegen complete. Generated files in gen/"
