#!/usr/bin/env bash
# Per-PR requirement coverage gate (docs/PLAN.md, "Definition of done").
#
# Usage: hack/duvet-coverage.sh ID [ID...]
#
# IDs are requirement identifiers such as SC-020. A range PREFIX-NNN..MMM (or
# PREFIX-NNN..PREFIX-MMM) expands to every identifier in the spec between the
# two numbers; identifiers explicitly listed have to exist.
#
# Runs `duvet report`, reads .duvet/reports/report.json and exits non-zero
# listing every identifier that lacks an implementation citation or a test
# citation. Set DUVET to point at a duvet binary, and SKIP_REPORT=1 to reuse
# an existing report.json.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
report="$root/.duvet/reports/report.json"
: "${DUVET:=duvet}"

if [ "$#" -eq 0 ]; then
  echo "usage: $0 ID [ID...]" >&2
  exit 2
fi
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

if [ "${SKIP_REPORT:-0}" != 1 ]; then
  if ! out=$(cd "$root" && rm -rf .duvet/requirements && "$DUVET" report 2>&1); then
    printf '%s\n' "$out" >&2
    echo "duvet report failed" >&2
    exit 2
  fi
fi
[ -f "$report" ] || { echo "missing $report" >&2; exit 2; }

# One line per requirement in the spec: "<ID> <spec-annotation-index>".
known=$(jq -r '
  .annotations
  | to_entries[]
  | select(.value.type == "SPEC")
  | (.value.comment // "" | capture("^- \\*\\*(?<id>[A-Z]+-[0-9]+)\\*\\*") | .id) as $id
  | "\($id) \(.key)"' "$report")

lookup() { # lookup ID -> annotation index or empty
  awk -v id="$1" '$1 == id { print $2; exit }' <<<"$known"
}

ids=()
for arg in "$@"; do
  for tok in ${arg//,/ }; do
    if [[ "$tok" =~ ^([A-Z]+)-([0-9]+)\.\.(([A-Z]+)-)?([0-9]+)$ ]]; then
      prefix=${BASH_REMATCH[1]}; from=$((10#${BASH_REMATCH[2]})); to=$((10#${BASH_REMATCH[5]}))
      if [ -n "${BASH_REMATCH[4]}" ] && [ "${BASH_REMATCH[4]}" != "$prefix" ]; then
        echo "bad range $tok: prefixes differ" >&2; exit 2
      fi
      for ((n = from; n <= to; n++)); do
        id=$(printf '%s-%03d' "$prefix" "$n")
        [ -n "$(lookup "$id")" ] && ids+=("$id")
      done
    else
      ids+=("$tok")
    fi
  done
done

failed=0
for id in "${ids[@]}"; do
  idx=$(lookup "$id")
  if [ -z "$idx" ]; then
    echo "FAIL $id: not a requirement in docs/requirements" >&2
    failed=1
    continue
  fi
  read -r citation test todo < <(jq -r --arg i "$idx" \
    '.statuses[$i] | "\(.citation // 0) \(.test // 0) \(.todo // 0)"' "$report")
  missing=()
  [ "$citation" -gt 0 ] || missing+=("implementation citation")
  [ "$test" -gt 0 ] || missing+=("test citation")
  if [ "${#missing[@]}" -gt 0 ]; then
    note=""
    [ "$todo" -gt 0 ] && note=" (only a todo annotation)"
    echo "FAIL $id: missing $(IFS="+"; echo "${missing[*]}" | sed "s/+/ and /")$note" >&2
    failed=1
  else
    echo "ok   $id"
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "coverage gate failed; see docs/requirements/README.md for the annotation syntax" >&2
  exit 1
fi
