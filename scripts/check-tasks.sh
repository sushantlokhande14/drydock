#!/usr/bin/env bash
# Checks that every bundled task is a fair task, before any agent sees it:
#   - the visible tests pass on the repository as shipped (the defect is not something a visible test already shows)
#   - the hidden tests fail on the repository as shipped (the defect is real)
#
# Run inside the sandbox image with the tasks directory mounted at /tasks:
#   docker run --rm -v "$PWD/tasks:/tasks:ro" -v "$PWD/scripts:/scripts:ro" drydock-sandbox:python bash /scripts/check-tasks.sh
set -u
status=0
for task in /tasks/*/; do
  name="$(basename "$task")"
  work="$(mktemp -d)"
  cp -r "$task/repo/." "$work/"
  cp -r "$task/hidden/." "$work/"
  cd "$work" || exit 1
  python -m pytest -q -p no:cacheprovider tests >/dev/null 2>&1
  visible=$?
  python -m pytest -q -p no:cacheprovider hidden_tests >/dev/null 2>&1
  hidden=$?
  if [ "$visible" -eq 0 ] && [ "$hidden" -eq 1 ]; then
    echo "ok    $name"
  else
    echo "BAD   $name (visible tests exit $visible, want 0; hidden tests exit $hidden, want 1)"
    status=1
  fi
done
exit $status
