#!/usr/bin/env bash
# PreToolUse hook for Bash.
#
# Three arms, all grounded in something measured here.
#
# 1. UNBOUNDED WAIT LOOPS - deny.
#    Of the agent runs that returned no result in one week, most contained
#    "[Request interrupted by user]", and the longest of them (88, 87, 70 and
#    44 minutes) were all sitting in `until ...; do sleep N; done` against a
#    build, a CI run or a log file. The agent was not dying; it was blocked, and
#    the owner killed it. Whatever it had not committed died with it.
#
#    The previous version of this guard let the command through if the string
#    "timeout" appeared anywhere in it. That was wrong twice: `timeout` and
#    `gtimeout` are NOT installed on this machine (macOS ships no coreutils
#    timeout, `command -v` finds neither), so the remedy it printed was a
#    command-not-found; and a trailing `# timeout` comment satisfied the test.
#
# 2. PUBLISHING PROSE OUTSIDE THE REPO - ask, with the rule attached.
#    Four confidently wrong figures shipped in one week, and a storage prefix
#    missing a path segment was about to be posted to a public issue. The advice
#    rides along with the prompt; the human still decides.
#
# 3. A SHELL WRITE TO A PATH THE ROUTE GUARD DENIES - deny.
#    route-guard.sh is a PreToolUse hook on Edit|Write|NotebookEdit, so it never
#    sees `sed -i`, `tee`, `cat > file`, `cp`, or a python one-liner. The two
#    strongest controls in the harness - the generated trees and an already
#    applied migration - were one heredoc away from being bypassed, with no
#    prompt and no record.
#
#    WHAT THIS CANNOT CLOSE, stated plainly because pretending otherwise is
#    worse than the gap: this is a text matcher over one command string. It
#    cannot see a path built from a variable, a path that arrives base64-encoded
#    or through `xargs`, a write performed by a script the command merely
#    invokes, an editor opened interactively, or `git apply` / `patch` of a diff
#    whose target paths never appear in the command line. A determined shell
#    write gets through. This raises the bar from "frictionless" to "deliberate
#    and visible", and that is the whole claim.
#
# FAIL-OPEN, DELIBERATELY, AND ANNOUNCED: see the header of route-guard.sh.
# session-brief.sh reports the guard as INACTIVE at session start if jq is
# absent.
#
# Test: scripts/claude/guards_test.sh
set -uo pipefail

command -v jq >/dev/null 2>&1 || exit 0

input=$(cat)
cmd=$(jq -r '.tool_input.command // empty' <<<"$input" 2>/dev/null)
[[ -z "$cmd" ]] && exit 0

emit() {
  jq -n --arg d "$1" --arg r "$2" '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: $d,
      permissionDecisionReason: $r,
      additionalContext: $r
    }
  }'
  exit 0
}

# ---- arm 1: unbounded wait loop -------------------------------------------
# A wait loop is any `while`/`until` that sleeps. It is BOUNDED only if the loop
# itself has an iteration ceiling: `for i in $(seq 1 N)` or a C-style `for ((`.
# Nothing about the word "timeout" is consulted, both because the binary does
# not exist here and because Claude Code strips `timeout` as a command wrapper
# before matching Bash permission rules anyway.
is_wait_loop=0
grep -qE '(^|[;&|[:space:]])(while|until)([[:space:]]|$)' <<<"$cmd" \
  && grep -qE '(^|[;&|[:space:]])sleep[[:space:]]' <<<"$cmd" \
  && is_wait_loop=1

if [[ $is_wait_loop -eq 1 ]]; then
  emit deny "Unbounded wait loop. A while/until loop that sleeps has no iteration
ceiling, so nothing ends it if the thing it waits for already exited or never
starts. That exact shape is what was running in this project's four longest
agent runs, all of which had to be killed by hand, losing every uncommitted
change.

\`timeout\` and \`gtimeout\` are NOT installed on this machine. Use one of:

  for i in \$(seq 1 60); do
    grep -q DONE /tmp/build.log && break
    sleep 10
  done                                  # bounded: 10 minutes, builtins only

  the Bash tool's own \`timeout\` parameter (milliseconds), which backgrounds
  the command rather than hanging

  the Monitor tool, for waiting on something you started with run_in_background

And before you wait on anything slow: commit what you have. A killed agent that
committed first loses nothing."
fi

# ---- arm 2: publishing prose outside the repo ------------------------------
if grep -qE '(^|[;&|[:space:]])gh[[:space:]]+(issue|pr)[[:space:]]+(create|comment|edit|close|review)' <<<"$cmd" \
   || grep -qE '(^|[;&|[:space:]])gh[[:space:]]+release[[:space:]]+(create|edit)' <<<"$cmd" \
   || grep -qE '(^|[;&|[:space:]])gh[[:space:]]+api[^|]*(comments|issues)' <<<"$cmd"; then
  emit ask "You are publishing prose outside this repository. Before it goes out:

- Never say \"fixed\" until it is merged AND deployed AND verified against the
  running system. Not at push, not at green CI, not at merge.
- Verify the thing, not the pipeline's report of it. A green deploy job says a
  container started. Query the deployed revision, download the published zip and
  grep it, list the objects.
- Recount every number with a command, in this turn. Four wrong figures shipped
  here in one week, each inherited rather than counted.
- Paste the exact path or prefix you are quoting, checked character by
  character. One was posted a path segment short.
- Say plainly what is NOT fixed, with the exact remedy, in the same message."
fi

# ---- arm 3: a shell write to a path route-guard.sh denies ------------------
# Only a WRITE counts. Reading, grepping or listing these paths is normal work,
# and a guard that blocks `grep -r x apps/api/internal/db/sqlc/ > /tmp/out`
# because the string appears somewhere in the line is the over-fire that gets
# the whole harness switched off. So each shape below identifies the protected
# path as the TARGET of the write, not merely as a substring of the command.
GENERATED='apps/api/internal/db/sqlc/|apps/api/internal/api/gen/|apps/web/src/routeTree\.gen\.ts|packages/openapi-client/src/generated/'
DEAD_APP='apps/landing/'

tok='[^[:space:];|&<>"'"'"']*'   # one shell word, unquoted

# A target belongs to THE COMMAND THAT WRITES IT, never to the line it sits on.
#
# Two defect shapes are closed here, both measured against this suite.
#
# (a) The LAST-TOKEN destination. cp/mv/rsync/install/truncate took
#     `awk '{print $NF}'` over the whole string, so
#     `cp secret.go apps/api/internal/db/sqlc/models.go # note` ended in `note`
#     and the write into the generated tree was permitted. awk also emits one
#     field PER LINE, so on a multi-line command the later grep fired on a
#     protected path that merely ended some unrelated line.
#
# (b) THREE UNCORRELATED GREPS. The in-place-editor, interpreter and rm arms
#     each asked "is `sed` anywhere in this string, is `-i` anywhere in this
#     string, is the path anywhere in this string" and denied on the
#     conjunction. Nothing tied the three to one command, so
#     `sed -n 1,5p apps/api/internal/db/sqlc/db.go | grep -i querier` was
#     denied: pure reading, with the `-i` belonging to grep. That is the
#     over-fire CLAUDE.md warns gets a guard switched off. It also under-fired:
#     `tee` was read positionally and only its FIRST operand was checked, so
#     `echo x | tee /tmp/a apps/api/internal/db/sqlc/db.go` was permitted while
#     tee writes to every operand.
#
# So every shape that needs correlation is decided in one pass over SEGMENTS:
# join `\`-continued lines, start a segment at each `;` `|` `&` (covering `&&`
# and `||`) and each newline, and inside a segment drop a trailing `#` comment
# and every redirection, skip leading VAR=value assignments and
# env/sudo/command wrappers, take the command name basename-wise so `/bin/cp`
# counts, and drop option words. What remains is that command's own operands:
#
#   cp mv rsync install   the LAST operand, or the `-t`/`--target-directory`
#                         value instead when given (not for rsync, where -t
#                         means preserve-times)
#   tee truncate          EVERY operand; both write to all of them
#   sed perl ruby         every operand, but only when THAT command carries an
#                         in-place flag (`-i`, `-i.bak`, `-pi`, `--in-place`)
#   python node php ruby  every operand, but only when THAT segment also
#                         contains a write call
#   rm, git rm            every operand, emitted as a DELETE rather than a
#                         write, because the dead-app arm permits deletion
#
# Redirection and `dd of=` are not listed: those two name their destination in
# the syntax itself, so they need no correlation and are matched directly.
#
# KNOWN LIMITS, stated rather than papered over: this splits on whitespace with
# no quote parsing, so a destination inside quotes, or built from a variable, is
# not seen; and the interpreter arm treats any `open(` as a write, so reading a
# protected path through python is refused. Both are conservative in the safe
# direction, and the header already declares the wider gap.
collect_dests() {
  local seg cn w f tflag inplace i n start
  local -a words operands
  while IFS= read -r seg; do
    seg=$(printf '%s' "$seg" \
      | sed -E -e 's/(^|[[:space:]])#.*$//' -e 's/[0-9]*[<>]+&?[[:space:]]*[^[:space:]]*//g')
    words=()
    IFS=$' \t' read -r -a words <<<"$seg"
    n=${#words[@]}
    [[ $n -eq 0 ]] && continue

    start=0
    while [[ $start -lt $n ]]; do
      case "${words[$start]}" in
        *=*|env|sudo|command|nohup|nice) start=$((start + 1)) ;;
        *) break ;;
      esac
    done
    [[ $start -ge $n ]] && continue

    cn=${words[$start]##*/}
    # `git rm` / `git mv` are the same operation one word further along.
    if [[ "$cn" == git && $((start + 1)) -lt $n ]]; then
      case "${words[$((start + 1))]}" in
        rm|mv) start=$((start + 1)); cn=${words[$start]} ;;
      esac
    fi
    case "$cn" in
      cp|mv|rsync|install|truncate|tee|sed|perl|ruby|python|python3|node|php|rm) ;;
      *) continue ;;
    esac

    operands=()
    tflag=
    inplace=
    i=$((start + 1))
    while [[ $i -lt $n ]]; do
      w=${words[$i]}
      case "$w" in
        --target-directory=*)
          case "$cn" in cp|mv|install) printf 'w:%s\n' "${w#*=}"; tflag=1 ;; esac ;;
        -t|--target-directory)
          case "$cn" in
            cp|mv|install)
              i=$((i + 1))
              [[ $i -lt $n ]] && printf 'w:%s\n' "${words[$i]}"
              tflag=1 ;;
          esac ;;
        --in-place|--in-place=*) inplace=1 ;;
        --*) ;;
        -*)
          # A short-flag bundle carrying `i` is the in-place flag: -i, -i.bak,
          # -pi. It is consulted only for sed/perl/ruby, so `cp -i` and `rm -i`
          # (interactive) set it harmlessly and are never read.
          f=${w#-}; f=${f%%.*}; f=${f%%=*}
          case "$f" in *i*) inplace=1 ;; esac ;;
        *) operands[${#operands[@]}]="$w" ;;
      esac
      i=$((i + 1))
    done
    [[ ${#operands[@]} -eq 0 ]] && continue

    case "$cn" in
      cp|mv|rsync|install)
        [[ -z "$tflag" ]] && printf 'w:%s\n' "${operands[$((${#operands[@]} - 1))]}" ;;
      tee|truncate)
        printf 'w:%s\n' "${operands[@]}" ;;
      rm)
        printf 'r:%s\n' "${operands[@]}" ;;
    esac
    case "$cn" in
      sed|perl|ruby)
        [[ -n "$inplace" ]] && printf 'w:%s\n' "${operands[@]}" ;;
    esac
    # ruby is in both lists deliberately: `ruby -i` edits in place and
    # `ruby -e 'File.write ...'` does not, and neither shape implies the other.
    case "$cn" in
      python|python3|ruby|node|php)
        grep -qE '(open\(|writeFile|file_put_contents|\.write\()' <<<"$seg" \
          && printf 'w:%s\n' "${operands[@]}" ;;
    esac
  done < <(printf '%s\n' "$cmd" \
           | awk '{ if (sub(/\\$/, "")) printf "%s", $0; else print }' \
           | tr ';|&' '\n\n\n')
}

# One target per line, tagged w (write) or r (delete), so no regex can match
# across two of them and the dead-app arm can permit deletion on its own.
DESTS=$(collect_dests)

# dest_matches <kind> <path-regex>
dest_matches() {
  [[ -z "$DESTS" ]] && return 1
  printf '%s\n' "$DESTS" | grep "^$1:" | grep -qE "$2"
}

# writes_to <path-regex> [allow_rm]
# Returns 0 if $cmd writes to something matching the regex.
writes_to() {
  local p="$1" allow_rm="${2:-no}"

  # cat > f, printf >> f, cmd 2> f, a heredoc's target: the syntax names it.
  grep -qE ">>?[[:space:]]*[\"']?${tok}(${p})" <<<"$cmd" && return 0
  # dd of=f: likewise.
  grep -qE "of=[\"']?${tok}(${p})" <<<"$cmd" && return 0
  # every target named by a write command, correlated to that command
  dest_matches w "$p" && return 0
  # deletion
  [[ "$allow_rm" == no ]] && dest_matches r "$p" && return 0
  return 1
}

if writes_to "$GENERATED"; then
  emit deny "That command writes to a GENERATED tree from the shell, which is the one
route around the Edit/Write guard. Hand-editing the sqlc tree caused a
production 500 here.

Regenerate instead:
  sqlc      \$(go env GOPATH)/bin/sqlc generate      (run in apps/api; not on PATH)
  ogen      cd apps/api && go generate ./internal/api/gen/...
  TS client pnpm -C packages/openapi-client generate
  routes    pnpm -C apps/web build

'make gen' and 'scripts/gen-openapi.sh' are STUBS and regenerate nothing.

Reading these paths is fine; only writing to them is refused."
fi

# The dead app: writing INTO it is always wrong, but deleting it is the correct
# end state, so `rm` is deliberately allowed here and nowhere else in this arm.
if writes_to "$DEAD_APP" allow_rm; then
  emit deny "That command writes into apps/landing, which is DEAD: last commit 2026-06-20,
absent from pnpm-workspace.yaml, superseded by apps/marketing. The directory is
still on disk, so the write will succeed at doing the wrong thing.

Deleting the directory is not blocked; only writing into it is."
fi

# An already-applied migration. Which files are applied is not a fixed list, so
# it is computed the same way route-guard.sh computes it: presence in HEAD.
if writes_to 'apps/api/migrations/'; then
  cwd=$(jq -r '.cwd // empty' <<<"$input" 2>/dev/null)
  root=""
  [[ -n "$cwd" && -d "$cwd" ]] && root=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null)
  if [[ -n "$root" ]]; then
    applied=""
    while IFS= read -r m; do
      [[ -z "$m" ]] && continue
      m="${m#./}"; m="${m#"$root"/}"
      git -C "$root" cat-file -e "HEAD:$m" 2>/dev/null && applied="$applied  $m"$'\n'
    done < <(grep -oE "${tok}apps/api/migrations/[A-Za-z0-9_.-]+\.sql" <<<"$cmd" | sort -u)

    if [[ -n "$applied" ]]; then
      emit deny "That command writes to a migration that already exists in HEAD:

${applied}
internal/db/migrate.go sorts versions lexically and skips anything already in
schema_migrations:

    sort.Strings(versions) ... if applied[version] { continue }

A database that ran this file will never read it again, so editing it changes
nothing and looks like a fix. A correction is a NEW ordinal plus a converge path
for databases on the earlier version. Route it to database-engineer.

Doing this from the shell instead of Edit does not change the outcome; it only
removes the record that it happened."
    fi
  fi
fi

exit 0
