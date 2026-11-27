#!/bin/sh
# The first package of the order not yet handed out this round. The
# package last handed out counts as migrated when its call gave it back;
# a call that failed gives no package.
set -eu
jq '(.done // []) as $done | .last as $last
  | ((.migrated // []) + (if $last != null and ($done | index($last)) then [$last] else [] end) | unique) as $migrated
  | ([.order[] | select(. as $p | $done | index($p) | not)] | first) as $p
  | if $p == null then {outcome: "finished", data: {done: $done, migrated: $migrated, skipped: ($done - $migrated)}}
    else {outcome: "package", data: {package: $p, done: ($done + [$p]), migrated: $migrated}} end'
