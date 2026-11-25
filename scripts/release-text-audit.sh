#!/usr/bin/env bash
# Ask git what a candidate artifact publishes before anybody names it as a roll target.
#
# Why this exists: every wrong claim this project has shipped into an operator-facing
# string arrived as a wording change that nobody was watching, and each one was already
# corrected somewhere else in this tree by the time a peer found it. The published text is
# therefore part of the release surface, and it is checkable in one command. maclet asked
# for exactly this habit after finding "errno 19 on Darwin is ENOTSUP" still published at
# build-35 and build-36, two commits after the sentence had been retracted in prose.
#
# Two independent questions, because they have different failure modes:
#   FALSIFIED    - does the artifact publish a claim measured false on the node?
#   ACCEPTANCE   - does it carry the strings a node install check greps for?
# A build can pass one and fail the other. build-30 publishes no false claim and still is
# not a valid target because it predates the probe that emits probe_status; build-40 passes
# the marker checks and publishes a claim we have all agreed is false.
set -u

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root" || exit 2

# Claims measured false on macOS 26.6.2 arm64. Keep the strings verbatim: they are matched
# against published source, and a paraphrase here checks nothing. This is not theoretical -
# the first version of this list carried 'does not register the null filesystem', which
# caught build-33 and build-34 and reported build-31 and build-32 clean, because those two
# say "the built kernel does not register the vfs at all". Same false claim, different words,
# and the audit lied about them. Every entry below was copied out of tagged source, not
# written from memory of what the claim meant.
falsified=(
  'errno 19 on Darwin is ENOTSUP'
  'does not register the null filesystem'
  'does not register the vfs at all'
  'registers no nullfs'
  'removed nullfs'
  'bindfs is the other bind-mount VFS Apple still ships'
)

# Substrings home-ops greps in the installed binary's guidance as a post-install acceptance
# check on icarus-2. Recorded in macgrubernetes f02a460, which pins build-41. Reword these
# only together with home-ops or their install check silently stops passing.
acceptance=(
  'errno 19 is ENODEV on both Darwin and Linux'
  'EPROGMISMATCH (75, which Linux numbers as EOVERFLOW) for nfs'
  'the type name bindfs is not registered at all'
)

target_file=cmd/macker/volume_capabilities.go

if [ "$#" -gt 0 ]; then
  refs=("$@")
  strict=1
else
  refs=()
  while read -r ref; do refs+=("$ref"); done < <(git tag --list 'build-*' --sort=-v:refname)
  strict=0
fi

printf '%-10s %-10s %-7s %s\n' 'REF' 'COMMIT' 'FILE' 'VERDICT'
failures=0
for ref in "${refs[@]}"; do
  sha=$(git rev-parse --verify --quiet "${ref}^{commit}")
  if [ -z "$sha" ]; then
    printf '%-10s %-10s %-7s %s\n' "$ref" '-' 'missing' 'UNRESOLVABLE REF'
    failures=$((failures + 1))
    continue
  fi
  if ! git cat-file -e "${sha}:${target_file}" 2>/dev/null; then
    printf '%-10s %-10s %-7s %s\n' "$ref" "${sha:0:10}" 'absent' 'NO CAPABILITY SOURCE: not a read-only release'
    [ "$strict" -eq 1 ] && failures=$((failures + 1))
    continue
  fi

  text=$(git show "${sha}:${target_file}")
  verdicts=()
  tripped=0
  for claim in "${falsified[@]}"; do
    if printf '%s\n' "$text" | grep -qF -- "$claim"; then
      verdicts+=("FALSIFIED: $claim")
      tripped=1
    fi
  done
  for want in "${acceptance[@]}"; do
    if ! printf '%s\n' "$text" | grep -qF -- "$want"; then
      verdicts+=("NO MARKER: $want")
      tripped=1
    fi
  done

  if [ "${#verdicts[@]}" -eq 0 ]; then
    printf '%-10s %-10s %-7s %s\n' "$ref" "${sha:0:10}" 'present' 'PUBLISHES NO KNOWN FALSE CLAIM, ALL MARKERS PRESENT'
  else
    printf '%-10s %-10s %-7s %s\n' "$ref" "${sha:0:10}" 'present' "$(IFS='; '; echo "${verdicts[*]}")"
  fi
  [ "$strict" -eq 1 ] && [ "$tripped" -eq 1 ] && failures=$((failures + 1))
done

echo
if [ "$strict" -eq 1 ]; then
  if [ "$failures" -eq 0 ]; then
    echo "OK: every named candidate is clean on both questions."
    exit 0
  fi
  echo "REFUSING: $failures candidate(s) publish a false claim or lack an acceptance marker."
  exit 1
fi
echo "Inventory mode: name the candidate explicitly to gate on it, for example $0 build-41"
