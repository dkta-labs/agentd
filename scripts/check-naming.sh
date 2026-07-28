#!/bin/sh
set -eu

retired_slug='agent''-gateway'
retired_title='Agent'' Gateway'
workstation_root='/Users/''dkta0/'

matches=$(git grep -n -I \
  -e "$retired_slug" \
  -e "$retired_title" \
  -e "$workstation_root" \
  -- . || true)

if [ -n "$matches" ]; then
  printf '%s\n' 'Canonical naming check failed:' >&2
  printf '%s\n' "$matches" >&2
  exit 1
fi

printf '%s\n' 'Canonical naming check passed.'
