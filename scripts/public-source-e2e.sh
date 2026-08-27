#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
go test ./internal/gate -run '^TestPublicSourceGateWorkerE2E$' -count=1
