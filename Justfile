# Development commands. Everything CI runs is a recipe here — the shared
# check workflow (truvity/ci-workflows) runs each one as its own job.

charts := "cloudflared docs-mcp gateway-fleet gateway-groups gateway-policies gateway-projects static-site static-site-infra"

# Library charts: they render nothing by themselves, so every check that
# needs a render goes through tests/harness/<chart>, the consumer chart
# that stands in for the application including them.
libraries := "gateway-routes"

# Lint every chart and the Go module.
#
# The schema is part of the lint: an unknown key must fail the render, not
# be silently ignored. Everything relational — two exposures claiming one
# name, a group claiming another group's hostname — is checked by the
# chart's own render-time validation, and every rule has a fixture under
# tests/invalid/<chart>/ that must fail.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    hack/sync-gateway-projects.sh check
    hack/cloudflared-pin.sh check
    hack/chart-deps.sh
    for chart in {{ charts }}; do
      # A chart that refuses to render without values of the consumer's (an
      # issuer, a digest) lists the smallest set that renders in
      # tests/lint/<chart>.yaml. Lint, the unknown-key probe and every
      # negative fixture start from it, so a fixture names only what it
      # breaks and cannot fail for the missing values instead.
      base=()
      if [ -f "tests/lint/$chart.yaml" ]; then base=(-f "tests/lint/$chart.yaml"); fi
      helm lint "charts/$chart" ${base[@]+"${base[@]}"}
      # An unknown top-level key must fail the render. Not `! helm
      # template ...`: bash's `set -e` ignores a command negated with `!`,
      # so such a probe could never fail the recipe.
      if helm template x "charts/$chart" ${base[@]+"${base[@]}"} --set bogusKey=1 >/dev/null 2>&1; then
        echo "$chart: an unknown key rendered" >&2
        exit 1
      fi
      # Every negative fixture must fail; one that renders is a hole in the
      # validation, which is exactly the kind of hole nobody notices.
      for values in tests/invalid/"$chart"/*.yaml; do
        if helm template invalid "charts/$chart" ${base[@]+"${base[@]}"} -f "$values" >/dev/null 2>&1; then
          echo "RENDERED BUT SHOULD HAVE FAILED: $values" >&2
          exit 1
        fi
      done
      echo "$chart: schema and $(ls tests/invalid/"$chart"/*.yaml | wc -l | tr -d ' ') negative fixtures OK"
    done
    for chart in {{ libraries }}; do
      helm lint "charts/$chart"
      # No values.schema.json can guard a library chart: its inputs are the
      # argument of a template, not values of a chart. The template refuses
      # an unknown key itself, and the fixtures below cover that rule like
      # any other.
      for values in tests/invalid/"$chart"/*.yaml; do
        if helm template invalid "tests/harness/$chart" -f "$values" >/dev/null 2>&1; then
          echo "RENDERED BUT SHOULD HAVE FAILED: $values" >&2
          exit 1
        fi
      done
      echo "$chart: $(ls tests/invalid/"$chart"/*.yaml | wc -l | tr -d ' ') negative fixtures OK"
    done
    golangci-lint config verify
    # GOTOOLCHAIN=local: go.mod's `toolchain` (go1.27.2, the stdlib security
    # fixes) leads devbox's go, and golangci-lint must typecheck with the go it
    # was built with. Drop with the toolchain lead.
    GOTOOLCHAIN=local golangci-lint run ./...

# Golden renders: render every test case and compare with tests/golden; then
# the Go tests (cloudflare/, catalog/).
test:
    hack/golden.sh
    go vet ./...
    go test ./...

# Regenerate the golden renders — review the diff before committing.
golden:
    hack/golden.sh update

# Reachable Go advisories (security.yaml, daily).
vuln:
    govulncheck ./...

# Compile check (library — nothing to run).
build:
    go build ./...

# Format Go files.
fmt:
    golangci-lint fmt ./...

# Run go mod tidy.
tidy:
    go mod tidy

# The reason this repository can be public. Runs in CI as its own job.
leak-canary:
    hack/leak-canary.sh

# Package every chart locally (the release workflow stamps the version from the tag).
package:
    #!/usr/bin/env bash
    set -euo pipefail
    hack/chart-deps.sh
    for chart in {{ charts }} {{ libraries }}; do helm package "charts/$chart" --destination dist/; done

# Everything CI runs on a pull request.
check: build lint test leak-canary
