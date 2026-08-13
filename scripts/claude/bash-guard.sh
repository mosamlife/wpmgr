#!/usr/bin/env bash
# PreToolUse hook for Bash.
#
# Four arms, all grounded in something measured here.
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
# 4. A GIT PUSH THAT WOULD LAND COMMITS ON main - deny.
#    On 2026-08-12 a one-line fix was committed on main in the main checkout and
#    pushed straight to origin/main, with no branch and no PR. Branch protection
#    on main carries four required contexts, but `enforce_admins` is
#    deliberately false, so an owner-token push is accepted server-side and not
#    one of those contexts ever ran against it. This hook saw that push and
#    permitted it: its entire notion of git was `git rm` and `git mv`. Nothing
#    server-side catches such a push, and the client-side lock that does,
#    `.githooks/pre-push`, lives in repo-local config that a fresh clone has not
#    run `make hooks` for yet. This arm is the early stop in front of it, which
#    is why it denies rather than asks, and why it also denies when it cannot
#    read the branch.
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
# not seen. That is conservative in the safe direction only for the OVER-fire
# half; it under-fires, and the header already declares that wider gap.
#
# This comment used to end "the interpreter arm treats any `open(` as a write,
# so reading a protected path through python is refused ... conservative in the
# safe direction". Both halves of that were wrong, and the second half was the
# damaging one: the arm's test was `(open\(|writeFile|file_put_contents|\.write\()`,
# which denied
#   python3 -c "print(open('apps/api/internal/db/sqlc/db.go').read())"   a READ
# and permitted
#   python3 -c "import shutil; shutil.copy('/tmp/x','apps/.../sqlc/db.go')"
# because a `shutil.copy` contains none of those four tokens. It refused the
# harmless half and waved through the overwrite of the tree whose hand-editing
# caused a production 500 here. A comment that claims safety in both directions
# is how that survived; the real boundary is stated at $IWRITE below.
# Strip ONE surrounding layer of quotes from a word, into $UNQ.
#
# `read -a` splits on whitespace and does not unquote, so `"apps/api/migrations"`
# arrives with its quotes still attached. The substring arms tolerated that,
# because their boundary class already treats a quote as a path boundary. The
# DERIVED path did not, and could not: `<dest>/<basename>` is handed to
# `git cat-file -e HEAD:<path>`, which needs the exact filename and got
# `apps/api/migrations"/"20260527115454_initial.sql`. So
#
#   cp "/tmp/20260527115454_initial.sql" "apps/api/migrations"
#
# overwrote an applied migration while the check looked for a path git could
# never resolve. One layer is all a shell removes, and setting a global rather
# than echoing keeps this off the subshell path in a loop that runs per operand.
unq() {
  UNQ=$1
  UNQ=${UNQ#\"}; UNQ=${UNQ%\"}
  UNQ=${UNQ#\'}; UNQ=${UNQ%\'}
}
UNQ=""

# The interpreter arm's two tests, and the honest statement of where their
# boundary is.
#
# WHAT THEY DECIDE: whether THIS segment performs a write (or a delete), not
# whether it mentions a file. An interpreter one-liner names its destination
# inside its own source text, so there is no positional operand to read; the
# only signal available is the call being made. So the calls are enumerated.
#
# WHAT THEY CANNOT DECIDE, since a comment that overstates coverage is worse
# than none: a call whose name arrives through a variable, an alias, `getattr`,
# `eval`, an imported helper, or a path assembled by a function call inside the
# `open(` argument list - `open(os.path.join('a','b'),'w')` is matched only by
# the `.write(` that usually follows it, and not at all without one. Where the
# shape is genuinely ambiguous the choice here is to DENY: `open(P,'r+')` is
# listed as a write because `r+` can write, and a bare `copy(`/`rename(` with
# no receiver is listed because PHP's are free functions. What is deliberately
# NOT denied is a plain read - `open(P)`, `open(P,'r')`, `read_text()`,
# `readFileSync` - because refusing those taught the previous version's users
# nothing except to route around it.
IQ='["'"'"']'
IWRITE="(shutil\.(copy|copy2|copyfile|copytree|move)\(\
|\.write_(text|bytes)\(\
|os\.(replace|rename|renames|truncate|ftruncate)\(\
|open\([^)]*(,|mode=)[[:space:]]*${IQ}[rwaxbt+U]*[wax+][rwaxbt+U]*${IQ}\
|\.write\(|\.writelines\(\
|writeFile|appendFile|createWriteStream|copyFile|renameSync|truncateSync\
|file_put_contents|fwrite|fputs|move_uploaded_file\
|(^|[^A-Za-z0-9_.\$>-])(copy|rename|touch)\()"
IDELETE="(rmtree\(\
|os\.(remove|removedirs|rmdir)\(\
|unlink(Sync)?\(\
|(^|[^A-Za-z0-9_])rm(dir)?(Sync)?\()"

collect_dests() {
  local seg cn w f d tdir inplace i n start probe
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
          case "$cn" in cp|mv|install) unq "${w#*=}"; tdir=$UNQ ;; esac ;;
        -t|--target-directory)
          case "$cn" in
            cp|mv|install)
              i=$((i + 1))
              [[ $i -lt $n ]] && { unq "${words[$i]}"; tdir=$UNQ; } ;;
          esac ;;
        --in-place|--in-place=*) inplace=1 ;;
        --*) ;;
        -*)
          # A short-flag bundle carrying `i` is the in-place flag: -i, -i.bak,
          # -pi. It is consulted only for sed/perl/ruby, so `cp -i` and `rm -i`
          # (interactive) set it harmlessly and are never read.
          f=${w#-}; f=${f%%.*}; f=${f%%=*}
          case "$f" in *i*) inplace=1 ;; esac ;;
        *) unq "$w"; operands[${#operands[@]}]="$UNQ" ;;
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
        # `.write(` first has its two stream forms removed, because
        # `sys.stdout.write(open(P).read())` is a read that prints and denying
        # it is exactly the over-fire that gets a guard switched off. Parameter
        # expansion, not a subshell: this runs once per segment.
        probe=${seg//sys.stdout.write(/}
        probe=${probe//sys.stderr.write(/}
        grep -qE "$IWRITE" <<<"$probe" && printf 'w:%s\n' "${operands[@]}"
        # Deletion is tagged r, the same as `rm`, so the one arm that permits a
        # delete (the dead app) keeps permitting it however it is spelled.
        grep -qE "$IDELETE" <<<"$probe" && printf 'r:%s\n' "${operands[@]}" ;;
    esac
  done < <(split_segments)
}

# One splitter, two callers. collect_dests decides write destinations with it
# and the git-push arm below decides command words with it; a second copy would
# drift, and this is the part every deny arm's correctness rests on. Defined
# after its caller deliberately - shell resolves a function at CALL time, and
# collect_dests is first called below both definitions.
split_segments() {
  printf '%s\n' "${1-$cmd}" \
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
      }'
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
  # FIRST: a destination that cannot be resolved to a specific file is not the
  # same answer as "no migration matched", and reading it as one was a silent
  # bypass of the strongest deny in this file.
  #
  # The literal scan further down extracts a filename with the character class
  # [A-Za-z0-9_.-]+\.sql, which contains no glob metacharacter. So
  #   sed -i.bak s/a/b/ apps/api/migrations/*.sql
  # matched NOTHING, $applied stayed empty, the `if` below was false, and
  # control fell straight to the bare `exit 0` at the end of this script: no
  # decision, no stdout, no stderr, and one command rewriting every committed
  # migration at once - the outage .claude/rules/db-migrations.md calls
  # non-negotiable, reached by adding a single `*`.
  #
  # THE GLOB IS NOT EXPANDED HERE, deliberately. This hook runs BEFORE the
  # command does, from a process whose cwd is not the payload's. What `*.sql`
  # names at guard time is not what it names at run time, so an expansion that
  # happened to find only new files would license a write that lands on an
  # applied one. The destination is judged on its SHAPE.
  #
  # DENY, not ask. Nothing honest reaches here. A new migration is created
  # under a literal timestamped name - that ordinal IS the file's identity, and
  # `cat > apps/api/migrations/20260813000000_x.sql` resolves fine. Reading the
  # directory (`ls`, `cat`, `grep` over `*.sql`) never enters this arm at all,
  # because none of those is a write and only write and delete destinations are
  # collected below. An ask would put a prompt in front of a shape that is
  # always wrong, and a prompt that is always answered the same way stops being
  # read.
  mig_targets() {
    # Redirection and `dd of=` name their destination in the syntax itself, and
    # collect_dests strips redirections, so those two are read from $cmd. Every
    # other destination is an already-correlated DESTS line, w (write) or r
    # (delete). READS are deliberately not scanned: writing a new migration and
    # then listing the directory,
    #   cat > apps/api/migrations/<new>.sql && ls apps/api/migrations/*.sql
    # is ordinary work, and taking the glob from the `ls` would redden it.
    grep -oE ">>?\|?[[:space:]]*[\"']?${lead}${MIGRATIONS}/${tok}" <<<"$cmd"
    grep -oE "of=[\"']?${lead}${MIGRATIONS}/${tok}" <<<"$cmd"
    printf '%s\n' "$DESTS" | grep -E '^[wr]:' | grep -oE "${MIGRATIONS}/${tok}"
  }
  unresolved=""
  while IFS= read -r m; do
    [[ -z "$m" ]] && continue
    m=${m##*apps/api/migrations/}
    # The bare directory, as `cp X apps/api/migrations/` emits it. collect_dests
    # emits the directory AND the derived `<dir>/<basename>`, and it is that
    # sibling entry which carries the name; flagging the directory itself would
    # deny every legitimate copy into it.
    [[ -z "$m" ]] && continue
    case "$m" in
      *'*'*|*'?'*|*'['*|*'{'*|*'$'*|*'`'*)
        unresolved="$unresolved  ${MIGRATIONS}/$m"$'\n' ;;
    esac
  done < <(mig_targets | sort -u)

  if [[ -n "$unresolved" ]]; then
    emit deny "That command writes under apps/api/migrations/ to a destination this guard
cannot resolve to a specific file:

${unresolved}
Whether a migration may be edited is decided by whether that exact file is in
HEAD, and a pattern has no exact file. It is not expanded here either: this hook
runs before the command does, so what the pattern matches now is not what it
will match when the shell runs it.

One glob over this directory rewrites every migration that has already run.
internal/db/migrate.go skips any version already in schema_migrations, so those
edits change nothing on an existing database and break every fresh install
instead - see .claude/rules/db-migrations.md.

Name the one file you mean. Creating a NEW migration under its own timestamped
name is not blocked. If the change really is to an existing migration, it is a
new ordinal plus a converge path, and it routes to database-engineer."
  fi

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
    # Normalise to the repo-relative path whatever shape it arrived in: the
    # DESTS tag, a `./` prefix, an absolute path, a quote the splitter left
    # behind. Everything before the last `apps/api/migrations/` is dropped, so
    # `git cat-file` is always asked a path it can actually resolve. Stripping
    # only a known prefix left `HEAD:w:apps/...` and `HEAD:/apps/...` being
    # asked, and both answer "not in HEAD" for a file that is.
    m="apps/api/migrations/${m##*apps/api/migrations/}"
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

# ---- arm 4: a push that would land commits on main -------------------------
# Decided per SEGMENT, from the same splitter every other arm uses: `git` has to
# be the command word and `push` its subcommand. So a push named inside a quoted
# string, inside a commit message or after a `#` is not a push, and neither is
# any command that merely contains the letters m-a-i-n.
#
# Branch names are compared by EQUALITY, after one layer of quotes comes off,
# never by substring or by regex. `main-ui`, `maintenance`, `feat/remaining`,
# `origin/main` as a log argument and `cp domain.go x` all have to stay ordinary
# work; a guard that reddens those gets switched off, and then main is guarded
# by nothing at all.
#
# WHAT IT CANNOT SEE, ENUMERATED, because a comment that overstates coverage is
# worse than no comment. All of these were reproduced against this arm, and each
# is PERMITTED by it under the condition written beside it:
#
#     eval git push origin main
#     bash -c "git push origin main"
#     git push origin $(echo main)
#     git p"ush" origin main                 the entry grep and unq() both miss it
#     git push origin @                      @ is git's documented synonym for HEAD
#     git push origin :                      the "matching" refspec
#     git push origin refs/heads/*:refs/heads/*
#     git -c push.default=matching push      ONLY from a checkout whose branch is
#                                            not main - the -c is consumed unread,
#                                            so the arm decides it as a bare push
#                                            and misses that matching can move a
#                                            main that is not the current branch.
#                                            From the main checkout it is DENIED,
#                                            for the ordinary reason.
#     (git push origin main)                 a subshell is not a segment here
#     { git push origin main; }              nor is a group
#     time git push origin main              nor is a timed command
#     if true; then git push origin main; fi a compound command's body
#
# That list is a SAMPLE, not an inventory, and it carries no count on purpose:
# every time someone looked it grew, and the number written here was stale
# within a day of being written. Deciding these means being the shell.
# `.githooks/pre-push` is what actually catches
# them: it runs inside git, after expansion and after refspec resolution, and is
# handed the refs that are about to move, so none of the above reaches it in
# disguise. This arm exists to refuse the ordinary shapes EARLY, with the rule
# attached, before a command runs at all. It is the manners, not the lock.

# push_branch <value of git -C, may be empty> -> $PBRANCH; 1 if unreadable.
#
# The branch has to come from the directory the command will actually run in,
# which is the hook payload's cwd, or from `git -C` when the command overrides
# it. The migration arm's SECOND source - this script's own directory - is
# deliberately not reused here, and that is not an oversight: this file is
# committed in the repository it guards, so from an agent worktree it resolves
# the MAIN checkout, whose HEAD is usually main, and every agent push in the
# project would be refused for a branch nobody was on. A wrong answer is worse
# than no answer; no answer is handled below, loudly.
#
# The base directory is PUSH_CWD, not the payload's cwd directly, because a
# leading `cd <path> &&` moves it. See the cd tracking in push_hits_main.
PBRANCH=""
push_branch() {
  local d="$1" base
  PBRANCH=""
  base=$PUSH_CWD
  if [[ -n "$d" ]]; then
    if [[ "$d" != /* ]]; then
      [[ -n "$base" ]] || return 1
      d="${base%/}/$d"
    fi
  else
    d="$base"
  fi
  [[ -n "$d" && -d "$d" ]] || return 1
  command -v git >/dev/null 2>&1 || return 1
  # --quiet exits non-zero on a detached HEAD rather than printing "HEAD", so a
  # detached checkout arrives here as unreadable instead of as a branch called
  # HEAD. Both are correct answers to "which branch"; only one of them is true.
  PBRANCH=$(git -C "$d" symbolic-ref --quiet --short HEAD 2>/dev/null)
  [[ -n "$PBRANCH" ]] || return 1
  return 0
}

# push_commands -> $cmd with the lines that are DATA rather than shell removed.
#
# Two shapes put the literal text `git push origin main` at the start of a line
# without any push being performed, and this arm denied both:
#
#   cat > docs/runbook.md <<EOF        a heredoc BODY is prose, not commands
#   To release, do NOT run
#   git push origin main
#   EOF
#
#   git commit -m "feat: a pre-push hook            a quoted string that spans
#                                                   lines is one argument, and
#   git push origin main is what the hook refuses.  the splitter resets its
#   "                                               quote state every line
#
# The second one is the shape of this change's own commit message, and the first
# is how every runbook in docs/ gets written. Both are correct work, and a guard
# that reddens correct work gets switched off.
#
# The splitter's per-line quote reset is deliberate and stays: for the WRITE arms
# an extra split can only over-fire, and over-firing there is caught by an
# assertion. Here it is the opposite, so this pass runs first and only for this
# arm. It drops a heredoc body up to its delimiter line, and drops the part of a
# line that belongs to a string opened on an EARLIER line - keeping whatever
# follows the closing quote, because `more" && git push origin main` really is a
# push and dropping the whole line would be a bypass.
#
# ITS OWN LIMIT: a heredoc whose delimiter never appears swallows the rest of the
# command. That command does not run in bash either - it waits for the delimiter
# on stdin - so there is nothing to guard; `.githooks/pre-push` is the backstop
# for it as for everything else here.
push_commands() {
  printf '%s\n' "$cmd" | awk '
    function scan(s,   i, n, c, rest, d) {
      n = length(s); i = 1
      while (i <= n) {
        c = substr(s, i, 1)
        if (c == "\\" && q != "'"'"'") { i += 2; continue }
        if (q != "") { if (c == q) q = ""; i++; continue }
        if (c == "'"'"'" || c == "\"") { q = c; i++; continue }
        if (c == "#" && (i == 1 || substr(s, i-1, 1) == " " || substr(s, i-1, 1) == "\t")) return
        if (c == "<" && substr(s, i+1, 1) == "<") {
          rest = substr(s, i + 2)
          # <<< is a here-STRING: it has no body and opens no delimiter.
          if (substr(rest, 1, 1) == "<") { i += 3; continue }
          sub(/^-/, "", rest); sub(/^[ \t]+/, "", rest)
          d = ""
          if (match(rest, /^"[^"]*"/))          d = substr(rest, RSTART + 1, RLENGTH - 2)
          else if (match(rest, /^'"'"'[^'"'"']*'"'"'/)) d = substr(rest, RSTART + 1, RLENGTH - 2)
          else if (match(rest, /^[A-Za-z0-9_.-]+/))  d = substr(rest, RSTART, RLENGTH)
          if (d != "") { nd++; dq[nd] = d }
          i += 2; continue
        }
        i++
      }
    }
    BEGIN { q = ""; nd = 0 }
    {
      line = $0
      if (nd > 0) {
        t = line; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t)
        if (t == dq[1]) { for (k = 1; k < nd; k++) dq[k] = dq[k+1]; nd-- }
        next
      }
      if (q != "") {
        idx = index(line, q)
        if (idx == 0) next
        q = ""
        line = substr(line, idx + 1)
      }
      scan(line)
      print line
    }'
}

# push_hits_main -> 0 and $PUSH_WHY set, when some segment is a push that lands
# on main or a push whose target cannot be read ($PUSH_WHY = UNKNOWN).
PUSH_WHY=""
PUSH_CWD=""
# Set when a `cd`/`pushd`/`popd` moved the shell somewhere this arm could not
# resolve. It means "the branch is not knowable from here", which is NOT the
# same as "the payload gave no cwd" - the second is still a deny, because a
# session with no cwd at all is a broken payload rather than an ordinary
# command. See the deny sites below.
PUSH_CD_UNKNOWN=""
PUSH_DIRSTACK=()
push_hits_main() {
  local seg w cn i n start sub d gitdir allrefs tagsonly dryrun r dst np cdto cdraw
  local -a words positional
  PUSH_CD_UNKNOWN=""
  PUSH_DIRSTACK=()
  PUSH_CWD=$(jq -r '.cwd // empty' <<<"$input" 2>/dev/null)
  while IFS= read -r seg; do
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
    unq "${words[$start]}"; cn=${UNQ##*/}

    # `cd <path> && git push` RETARGETS the branch lookup. This is the incident
    # shape and it is why the arm was worth repairing at all: an agent works in a
    # worktree, so the payload's cwd is a worktree, and
    #   cd /Users/.../wpmgr && git push
    # was permitted with zero bytes transferred to the guard's attention while
    # `git -C /Users/.../wpmgr push` was denied. It is the same push.
    #
    # It cuts the other way too, and that half was a FALSE REASON rather than a
    # bypass: from the main checkout, `cd <worktree> && git push` was refused
    # saying "the current branch is main". It was not. Either follow the cd or
    # stop claiming to know the branch.
    #
    # An operand this cannot resolve - `cd $DIR`, `cd "$(git ...)"`, `cd -`,
    # `cd ~x`, a directory a previous segment creates at run time - is recorded
    # as NOT KNOWN rather than guessed at. It used to clear the directory and
    # fall into the UNKNOWN deny, and that denied five shapes of correct work,
    # all reproduced from a worktree whose HEAD was not main:
    #
    #     cd "$(git rev-parse --show-toplevel)" && git push   the worktree ITSELF
    #     cd ~/Desktop/Terminal/wpmgr && git push             tilde, never expanded
    #     pushd /tmp >/dev/null && ls && popd && git push      popd, never tracked
    #     mkdir -p out && cd out && cd .. && git push          `out` not there yet
    #
    # `~` and `popd` are now followed, because both are small and exact. What is
    # left unresolvable DEFERS: no deny, no reason, and `.githooks/pre-push`
    # decides it instead, after the shell has actually expanded it. That is the
    # trade this file already makes everywhere else - a false reason is worse
    # than no reason, and the hook is the lock while this arm is the manners.
    if [[ "$cn" == cd || "$cn" == pushd || "$cn" == popd ]]; then
      if [[ "$cn" == popd ]]; then
        if [[ ${#PUSH_DIRSTACK[@]} -gt 0 ]]; then
          PUSH_CWD=${PUSH_DIRSTACK[${#PUSH_DIRSTACK[@]}-1]}
          unset 'PUSH_DIRSTACK[${#PUSH_DIRSTACK[@]}-1]'
          [[ -n "$PUSH_CWD" ]] || PUSH_CD_UNKNOWN=1
        else
          # popd with nothing pushed is an error in bash and moves nothing, but
          # this arm may have missed the pushd, so it declines to know.
          PUSH_CWD=""; PUSH_CD_UNKNOWN=1
        fi
        continue
      fi
      cdto=""; cdraw=""
      i=$((start + 1))
      while [[ $i -lt $n ]]; do
        unq "${words[$i]}"; w=$UNQ
        case "$w" in
          # `--` ends options, so the NEXT word is the operand even when it
          # starts with a dash. `cd -- /path` stays fully resolved; `cd --` with
          # nothing after it falls out with $cdto empty, into the branch below.
          --) i=$((i + 1))
              if [[ $i -lt $n ]]; then unq "${words[$i]}"; cdto=$UNQ; cdraw=${words[$i]}; fi
              break ;;
          -*) ;;
          *) cdto=$w; cdraw=${words[$i]}; break ;;
        esac
        i=$((i + 1))
      done
      [[ "$cn" == pushd ]] && PUSH_DIRSTACK+=("$PUSH_CWD")
      # Every operandless form declines to know, which is what the paragraph
      # above already promised and what these three did NOT do. Measured from a
      # worktree, before this: `cd - && git push`, `cd -- && git push` and
      # `cd && git push` were all resolved to $HOME and then DENIED with "could
      # not determine which branch", a reason that was untrue of the command -
      # and the symmetric half is worse, because with $OLDPWD or $HOME being a
      # branch checkout the same code PERMITS a push that lands on main.
      #   cd -   goes to $OLDPWD, which is the shell's, not this process's.
      #   cd --  and bare `cd` go to $HOME, which is not where the shell is.
      #   bare pushd swaps the top two entries, which is not modelled.
      # None of the four is knowable from the command string, so all four defer
      # and `.githooks/pre-push` decides them after the shell has expanded them.
      if [[ -z "$cdto" ]]; then
        PUSH_CWD=""; PUSH_CD_UNKNOWN=1
        continue
      fi
      # Tilde expansion happens only on an UNQUOTED leading ~, so the RAW word
      # is what decides it: bash does not expand "~/x", and neither does this.
      case "$cdraw" in
        '~')   cdto=${HOME:-} ;;
        '~/'*) cdto="${HOME:-}/${cdraw#\~/}" ;;
      esac
      # A RELATIVE target with no base is not resolvable, and resolving it
      # against THIS SCRIPT's own cwd is the worst available answer: from a
      # session inside .claude/worktrees, `cd out && cd ..` then landed on the
      # main checkout and denied with "the current branch is main", about a
      # directory the command never visited.
      if [[ "$cdto" != /* ]]; then
        if [[ -n "$PUSH_CWD" ]]; then
          cdto="${PUSH_CWD%/}/$cdto"
        else
          PUSH_CWD=""; PUSH_CD_UNKNOWN=1; continue
        fi
      fi
      if [[ -d "$cdto" ]]; then
        PUSH_CWD=$(cd "$cdto" 2>/dev/null && pwd -P) || PUSH_CWD=""
        [[ -n "$PUSH_CWD" ]] || PUSH_CD_UNKNOWN=1
      else
        PUSH_CWD=""; PUSH_CD_UNKNOWN=1
      fi
      continue
    fi

    [[ "$cn" == git ]] || continue

    # git's own options come BEFORE the subcommand, and two of them change which
    # repository the push reads its branch from. `-C dir` is followed, so
    # `git -C <main checkout> push` from a worktree is still a push to main;
    # --git-dir/--work-tree/--namespace are not followed, so the branch becomes
    # unreadable rather than silently taken from the wrong tree. Their separate
    # -value forms consume that value, or the value itself would be read as the
    # subcommand and the whole push would be skipped.
    d=""; gitdir=""; sub=""
    i=$((start + 1))
    while [[ $i -lt $n ]]; do
      unq "${words[$i]}"; w=$UNQ
      case "$w" in
        -C) i=$((i + 1)); [[ $i -lt $n ]] && { unq "${words[$i]}"; d=$UNQ; } ;;
        -c) i=$((i + 1)) ;;
        --git-dir=*|--work-tree=*|--namespace=*) gitdir=1 ;;
        --git-dir|--work-tree|--namespace) gitdir=1; i=$((i + 1)) ;;
        -*) ;;
        *) sub=$w; break ;;
      esac
      i=$((i + 1))
    done
    [[ "$sub" == push ]] || continue

    positional=()
    allrefs=""; tagsonly=""; dryrun=""
    i=$((i + 1))
    while [[ $i -lt $n ]]; do
      unq "${words[$i]}"; w=$UNQ
      case "$w" in
        # --dry-run/-n transfers NOTHING. Refusing it was a defect twice over:
        # the reason printed - "That push lands commits on main" - was untrue of
        # the command it refused, and it blocked the one safe way to inspect the
        # dangerous command before running it. Checking what a push would do is
        # the behaviour to encourage, not to redden.
        #
        # The abbreviations are here because git's parse-options accepts any
        # UNAMBIGUOUS prefix, so `git push --dry origin main` really is a dry
        # run and really transfers nothing - and it was denied, with the reason
        # "that push lands commits on main", which is untrue of it. Listed down
        # to `--dr`, which is the shortest prefix no other push option shares:
        # `--d` is ambiguous with --delete and git rejects it itself.
        --dry-run|--dry-ru|--dry-r|--dry-|--dry|--dr|-n) dryrun=1 ;;
        --all|--mirror) allrefs=1 ;;
        # --tags with no refspec pushes refs/tags and nothing else, so it moves
        # no branch. Exact match: --follow-tags is an ordinary branch push.
        --tags) tagsonly=1 ;;
        # These take a separate value, which must not be read as a refspec.
        -o|--push-option|--repo|--receive-pack|--exec) i=$((i + 1)) ;;
        -*) ;;
        *) positional[${#positional[@]}]="$w" ;;
      esac
      i=$((i + 1))
    done

    [[ -n "$dryrun" ]] && continue

    if [[ -n "$allrefs" ]]; then
      PUSH_WHY="--all/--mirror pushes every local branch, and one of them is main"
      return 0
    fi

    # `git push [<repository> [<refspec>...]]`: the FIRST positional is always
    # the repository, whether it is a remote name or a URL.
    np=${#positional[@]}
    if [[ $np -gt 1 ]]; then
      i=1
      while [[ $i -lt $np ]]; do
        r=${positional[$i]}
        r=${r#+}                # `+main` is a force-push of main
        dst=${r##*:}            # `src:dst` lands on dst; no colon means dst = src
        dst=${dst#refs/heads/}
        if [[ "$dst" == main ]]; then
          PUSH_WHY="the refspec '${positional[$i]}' targets main on the remote"
          return 0
        fi
        # `git push origin HEAD` lands on a remote branch named for the CURRENT
        # branch, so it is only readable by asking which branch that is.
        if [[ "$dst" == HEAD && "$r" != *:* ]]; then
          if [[ -n "$gitdir" ]] || ! push_branch "$d"; then
            # Unreadable BECAUSE a cd could not be followed: defer to the hook
            # rather than deny with a reason that is not true of the command.
            [[ -n "$PUSH_CD_UNKNOWN" && -z "$gitdir" && "$d" != /* ]] && continue 2
            PUSH_WHY="UNKNOWN"
            return 0
          fi
          if [[ "$PBRANCH" == main ]]; then
            PUSH_WHY="HEAD is the current branch and the current branch is main"
            return 0
          fi
        fi
        i=$((i + 1))
      done
      continue
    fi

    # No refspec at all: the push goes to the current branch.
    [[ -n "$tagsonly" ]] && continue
    if [[ -n "$gitdir" ]] || ! push_branch "$d"; then
      # Same deferral as above: an unfollowable cd is a gap in this arm's
      # knowledge, not evidence about the branch. The hook decides it.
      [[ -n "$PUSH_CD_UNKNOWN" && -z "$gitdir" && "$d" != /* ]] && continue
      PUSH_WHY="UNKNOWN"
      return 0
    fi
    if [[ "$PBRANCH" == main ]]; then
      PUSH_WHY="it names no refspec, so it pushes the current branch, and the current branch is main"
      return 0
    fi
  done < <(split_segments "$(push_commands)")
  return 1
}

# A cheap entry gate: the structural test costs two awk processes, and most
# commands are not pushes. It NEVER decides anything by itself.
#
# It is not sound, and the previous comment here claimed it was - "the word push
# cannot be absent from a git push". `git p"ush" origin main` has no word `push`
# in it, is a real push, and is permitted by this grep and by unq() alike. That
# is not fixed here, on purpose: see the enumerated list above. The lock is
# `.githooks/pre-push`, which sees the resolved ref and does not care how the
# word was spelled.
if grep -qE '(^|[^A-Za-z0-9_-])push([^A-Za-z0-9_-]|$)' <<<"$cmd" && push_hits_main; then
  if [[ "$PUSH_WHY" == UNKNOWN ]]; then
    push_cwd=$(jq -r '.cwd // empty' <<<"$input" 2>/dev/null)
    emit deny "That is a git push, and this guard COULD NOT DETERMINE which branch it would
land on, so it cannot rule out main.

The branch is read from the directory the command runs in, and that failed here:
the hook payload's cwd was '${push_cwd:-<none>}', and one of these is true - it
is absent, it is not a directory, it is not inside a worktree, HEAD is detached,
git is not on PATH, or the command overrode the repository with --git-dir /
--work-tree / --namespace.

A push this guard cannot read is not a push it may allow. Branch protection on
main has enforce_admins deliberately false, so nothing after this hook checks
anything: if this one is wrong, the commits are on origin/main.

Name the target and it needs no lookup at all:

  git push origin <branch>        any branch except main
  git push -u origin fix/<name>

Or re-run it from inside the checkout the push belongs to."
  fi
  emit deny "That push lands commits on main, and the only route to main is a pull request:
${PUSH_WHY}.

CLAUDE.md, \"## Delivery\": branch, push the branch, open the PR, let ci.yml and
review run, then merge. Never git push while HEAD is main - not for a one-line
fix, not for a typo, not because CI will pass anyway. Approval has to precede the
irreversible half, and a push to origin/main IS the irreversible half.

Nothing server-side backs this up: branch protection on main has enforce_admins
deliberately false, so an owner-token push is accepted and its required contexts
never run against it. The enforcement is client-side. .githooks/pre-push is the
lock, since git hands it the resolved refs - but it is repo-local config, so each
clone needs 'make hooks'. This PreToolUse hook is the early stop in front of it,
matching command text. Both catch an accident; neither stops a determined push.

  git switch -c fix/<name>
  git push -u origin fix/<name>
  gh pr create

Pushing a branch, a tag, or a release branch is untouched by this arm; only a
push that moves main is refused."
fi

exit 0
