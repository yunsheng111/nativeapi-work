#!/bin/sh
# upstream-check.sh 拉取上游（origin）并列出本地 main 与它的差异面：
#   1) 本地落后的提交清单；2) 上游改动的文件；3) 双方都改过、合并时会冲突的文件。
# 只读操作，不改工作区、不合并；合并前的"试合并"用：
#   git merge-tree --write-tree --name-only main origin/main
set -e
cd "$(dirname "$0")/.."

git fetch origin --tags

BASE=$(git merge-base main origin/main)
echo "=== 分叉点: $BASE"
echo
echo "=== 上游新提交（本地 main 落后 $(git log main..origin/main --oneline | wc -l) 条）==="
git log main..origin/main --oneline
echo
echo "=== 本地独有提交（origin/main 没有的 $(git log origin/main..main --oneline | wc -l) 条）==="
git log origin/main..main --oneline
echo
echo "=== 上游改动文件（git diff main...origin/main）==="
git diff main...origin/main --stat | tail -30
echo
echo "=== 双方都改过的文件（合并冲突候选）==="
git diff --name-only "$BASE" origin/main | sort > "$HOME/.upstream-files.$$"
git diff --name-only "$BASE" main | sort > "$HOME/.local-files.$$"
comm -12 "$HOME/.upstream-files.$$" "$HOME/.local-files.$$" || true
rm -f "$HOME/.upstream-files.$$" "$HOME/.local-files.$$"
echo
echo "=== 无副作用试合并（列出真实冲突文件）==="
git merge-tree --write-tree --name-only main origin/main 2>&1 | sed -n '2p;/CONFLICT/p' || true
echo
echo "提示：确认要合并时执行  git merge origin/main  （解决冲突后 go vet ./... && go test ./...，再构建部署）"
