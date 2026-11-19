#!/bin/sh
# The repository is public: checks that files tracked by git contain nothing personal
# or internal. Run by `make check`. Fails (exit 1) if a tracked file:
#  - is a personal working file (agent instructions, plan, task tracking);
#  - refers to one, or cites an internal task identifier (T followed by 3 digits);
#  - contains a home path, a real e-mail address or the author's git e-mail.
# Those personal files stay local, ignored through .git/info/exclude.
set -eu
cd "$(git rev-parse --show-toplevel)"

fail=0
err() { echo "public-check: FAILED: $*" >&2; fail=1; }
show() { [ -n "$1" ] && { err "$2"; echo "$1" >&2; }; return 0; }

personal='^(CLAUDE\.md|AGENTS\.md|PLAN\.md|tasks/|docs/progress/|docs/decisions\.md|docs/joal-reference\.md|\.claude/|\.cursor|\.aider|\.windsurfrules)'
show "$(git ls-files | grep -E "$personal" || true)" "personal file tracked by git (git rm --cached):"

show "$(git grep -n -I -E 'PLAN\.md|AGENTS\.md|CLAUDE\.md|tasks/T|docs/progress|decisions\.md|joal-reference|\.claude/' -- . ':!tools/public-check.sh' || true)" \
	"reference to a personal file:"
show "$(git grep -n -I -E '(^|[^A-Za-z0-9_])T0[0-9]{2}([^0-9]|$)' -- . ':!tools/public-check.sh' || true)" "internal task identifier:"

# Personal data. Exceptions: generic examples (/home/user...), noreply@..., example or
# reserved domains (example.com|org|net, .example, .test, .invalid).
show "$(git grep -n -I -E '/(Users|home)/[^/ ]+' -- . ':!tools/public-check.sh' \
	| grep -v -E '/(Users|home)/(user|username|you|name)([/ ]|$)' || true)" "personal path:"
show "$(git grep -n -I -o -E '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' -- . ':!tools/public-check.sh' \
	| grep -v -i -E 'noreply|@(([A-Za-z0-9-]+\.)*example\.(com|org|net)|[A-Za-z0-9.-]*\.(example|test|invalid))$' || true)" "e-mail address:"
# The author's git e-mails (local and global: the repository may use a noreply address).
for email in $(git config --local user.email || true) $(git config --global user.email || true); do
	show "$(git grep -n -I -F -i -e "$email" -- . || true)" "author's git e-mail present:"
done

[ "$fail" -eq 0 ] || exit 1
echo "public-check: OK"
