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
#
# THE PREFIXES CARRY NO TRAILING SLASH, and that is the whole point. Every
# entry used to end in `/`, so a protected path was only ever recognised when
# something followed it. A DIRECTORY destination has nothing following it, and a
# directory destination is the ordinary shape of a copy:
#
#   cp -r /tmp/newsqlc apps/api/internal/db/sqlc      permitted
#   mv /tmp/models.go  apps/api/internal/db/sqlc      permitted
#   cp -t apps/api/internal/db/sqlc /tmp/models.go    permitted
#   cp -r /tmp/newlanding apps/landing                permitted
#   cp /tmp/20260531050000_m19_orgs_sharing.sql apps/api/migrations   permitted
#
# All five land a real file in a real protected directory in this checkout. The
# last one overwrote an ALREADY-APPLIED migration and never even reached the
# HEAD check, because the arm's own `writes_to` returned false first.
#
# Dropping the slash needs a boundary in its place, or `apps/api/migrations`
# starts matching `apps/api/migrations-notes.txt` and `apps/landing` starts
# matching `apps/landing-old`. BOUND is that boundary: a protected prefix
# matches only where the path ends or a new component begins. It is a path
# boundary, never a substring.
GENERATED='apps/api/internal/db/sqlc|apps/api/internal/api/gen|apps/web/src/routeTree\.gen\.ts|packages/openapi-client/src/generated'
DEAD_APP='apps/landing'
MIGRATIONS='apps/api/migrations'

tok='[^[:space:];|&<>"'"'"']*'   # one shell word, unquoted
# A protected prefix must START a path component and must END one. Without the
# trailing test, `apps/api/migrations` matches `apps/api/migrations-notes.txt`
# and `apps/landing` matches `apps/landing-old`; without the leading test, a
# sibling tree such as `myapps/api/internal/db/sqlc` matches. Both are path
# boundaries, not word boundaries: `/` is what separates components.
BOUND='(/|[[:space:]]|["'"'"']|\)|,|;|\||&|$)'
LEADB='(^|[:/"'"'"'(,=[:space:]])'
# An optional leading directory, for the two arms that match inside the raw
# command string rather than against an already-extracted target.
lead="(${tok}/)?"

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
  local seg cn w f d tdir inplace i n start
  local -a words operands
  while IFS= read -r seg; do
    # Comments and separators are already handled, quote-aware, by the splitter
    # below. Only redirections are stripped here.
    seg=$(printf '%s' "$seg" \
      | sed -E -e 's/[0-9]*[<>]+&?[[:space:]]*[^[:space:]]*//g')
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
    tdir=
    inplace=
    i=$((start + 1))
    while [[ $i -lt $n ]]; do
      w=${words[$i]}
      case "$w" in
        --target-directory=*)
          case "$cn" in cp|mv|install) tdir=${w#*=} ;; esac ;;
        -t|--target-directory)
          case "$cn" in
            cp|mv|install)
              i=$((i + 1))
              [[ $i -lt $n ]] && tdir=${words[$i]} ;;
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
        # The destination may be a FILE or a DIRECTORY, and which one it is
        # cannot be resolved here: the payload's cwd is the agent's, not this
        # process's, and the tree may not even exist yet. So emit both readings.
        # `cp X apps/api/migrations` lands `apps/api/migrations/X`, and that
        # resolved name is what the applied-migration check has to see - the
        # command line itself never contains it.
        if [[ -n "$tdir" ]]; then
          printf 'w:%s\n' "$tdir"
          for w in "${operands[@]}"; do printf 'w:%s/%s\n' "${tdir%/}" "${w##*/}"; done
        else
          d=${operands[$((${#operands[@]} - 1))]}
          printf 'w:%s\n' "$d"
          i=0
          while [[ $i -lt $((${#operands[@]} - 1)) ]]; do
            printf 'w:%s/%s\n' "${d%/}" "${operands[$i]##*/}"
            i=$((i + 1))
          done
        fi ;;
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
           | awk '
      # Split into command segments and drop comments, both QUOTE-AWARE. A
      # plain `tr ";|&"` plus a `s/ #.*//` cut inside quoted text, so
      #   sed -i "s| #x|y|" apps/api/internal/db/sqlc/db.go
      # lost its destination to a `|` and a `#` that were data, and the write
      # was permitted.
      #
      # BACKSLASH ESCAPES ARE CONSUMED IN PAIRS. The first version of this
      # splitter had none, and that was a REGRESSION worse than the nit it
      # fixed: `\` is a literal apostrophe to bash, but it opened a quote state
      # here that never closed, so
      #   echo don\'"'"'t; cp /tmp/x apps/api/internal/db/sqlc/db.go
      # became ONE segment whose command word was `echo`, and the write was
      # never seen. That is reachable from any apostrophe in ordinary prose in
      # a chained command, and it defeated all three deny arms.
      #
      # Inside SINGLE quotes bash performs no escaping at all - a backslash is
      # a literal character and the next `'"'"'` still closes the string - so the
      # pairing is skipped there, matching the shell rather than guessing.
      #
      # WHEN IN DOUBT, SPLIT. If a line ends with a quote still open, this
      # cannot know where the command ends, so it falls back to the old
      # separator split for that line. A spurious split can only over-fire,
      # which an assertion catches; an absorbed segment is a silent bypass. The
      # same reasoning covers the per-line quote reset: line 2 of a genuine
      # multi-line quoted string is parsed as unquoted, which splits more, not
      # less.
      {
        q = ""; out = ""; prev = ""
        n = length($0)
        for (i = 1; i <= n; i++) {
          c = substr($0, i, 1)
          if (c == "\\" && q != "'"'"'") {
            d = substr($0, i + 1, 1)
            out = out c d
            i++
            prev = d
            continue
          }
          if (q != "") { out = out c; if (c == q) q = "" }
          else if (c == "'"'"'" || c == "\"") { q = c; out = out c }
          else if (c == "#" && (prev == "" || prev == " " || prev == "\t")) break
          else if (c == ";" || c == "|" || c == "&") out = out "\n"
          else out = out c
          prev = c
        }
        if (q != "") { line = $0; gsub(/[;|&]/, "\n", line); print line }
        else print out
      }')
}

# One target per line, tagged w (write) or r (delete), so no regex can match
# across two of them and the dead-app arm can permit deletion on its own.
DESTS=$(collect_dests)

# dest_matches <kind> <path-regex>
# The prefix is matched as a whole path component, never as a substring of a
# longer one. It is NOT anchored at the start of the target: the interpreter arm
# emits a whole quoted expression as its operand, so the path sits inside
# `open('...','w')` rather than beginning the token.
dest_matches() {
  [[ -z "$DESTS" ]] && return 1
  printf '%s\n' "$DESTS" | grep "^$1:" | grep -qE "${LEADB}($2)${BOUND}"
}

# writes_to <path-regex> [allow_rm]
# Returns 0 if $cmd writes to something matching the regex.
writes_to() {
  local p="$1" allow_rm="${2:-no}"

  # cat > f, printf >> f, cmd 2> f, a heredoc's target: the syntax names it.
  # `>|` is a redirection too (it overrides noclobber), and without the `\|?`
  # the segment split then orphaned the path and the write was permitted.
  grep -qE ">>?\|?[[:space:]]*[\"']?${lead}(${p})${BOUND}" <<<"$cmd" && return 0
  # dd of=f: likewise.
  grep -qE "of=[\"']?${lead}(${p})${BOUND}" <<<"$cmd" && return 0
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
if writes_to "$MIGRATIONS"; then
  # Resolve a repository to ask, or refuse. This arm used to leave `root` empty
  # whenever `.cwd` was absent from the payload or was not inside a worktree,
  # and then simply skip the whole check: no decision, no message, command
  # ALLOWED. That is a silent route around the strongest deny in this file,
  # available to any payload that happens not to carry a cwd, and silence is
  # the one outcome a guard may never produce. A check that cannot do its job
  # says so.
  cwd=$(jq -r '.cwd // empty' <<<"$input" 2>/dev/null)
  root=""
  [[ -n "$cwd" && -d "$cwd" ]] && root=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null)
  # Second source, so a missing cwd does not turn into a blanket refusal of
  # ordinary new-migration work: this hook script is itself committed in the
  # repository it guards, so its own directory resolves the same checkout.
  if [[ -z "$root" ]]; then
    self_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd -P) || self_dir=""
    [[ -n "$self_dir" ]] && root=$(git -C "$self_dir" rev-parse --show-toplevel 2>/dev/null)
  fi

  if [[ -z "$root" ]]; then
    emit deny "That command writes under apps/api/migrations/, and this guard cannot resolve
any git repository to check it against: the hook payload carried no usable cwd
(got '${cwd:-<none>}'), and this script's own directory is not inside a
worktree either.

Whether the target is an ALREADY-APPLIED migration is decided by whether it
exists in HEAD, and with no repository that question has no answer. This arm
used to fall silent here and let the command run, which is a route around the
deny rather than an answer to it, so it refuses instead.

Re-run the command from inside the checkout, or route the change to
database-engineer."
  fi

  applied=""
  while IFS= read -r m; do
    [[ -z "$m" ]] && continue
    m="${m#w:}"; m="${m#r:}"   # strip the DESTS tag, or HEAD:w:apps/... is asked
    m="${m#./}"; m="${m#"$root"/}"
    git -C "$root" cat-file -e "HEAD:$m" 2>/dev/null && applied="$applied  $m"$'\n'
  done < <(printf '%s\n%s\n' "$cmd" "$DESTS" \
           | grep -oE "${tok}apps/api/migrations/[A-Za-z0-9_.-]+\.sql" | sort -u)
  # $DESTS as well as $cmd, because `cp X apps/api/migrations` names the target
  # file nowhere on the command line: the name comes from the SOURCE basename,
  # and collect_dests is what resolves it. Scanning only $cmd let exactly that
  # shape overwrite an applied migration.

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

exit 0
