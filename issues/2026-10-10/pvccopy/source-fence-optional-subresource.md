# Source fence must handle absent admission subResource

Priority: P1. Status: corrected locally; repeat live positive helper acceptance pending.

Live Kubernetes 1.37 accepted/type-checked the policy but omitted request.subResource on a normal Pod CREATE. Direct access caused a CEL runtime error; FailurePolicy Fail denied the legitimate read-only helper. Negative denials alone did not prove the copy path works.

Codex prompt: guard absence with has(request.subResource), while rejecting nonempty subresources. Retain exact source/Runner/image and fsGroup/SELinux/read-only constraints. Regenerate installer templates, update only the UID-owned disposable fixture policy, wait for observed generation/typeChecking, repeat unsafe denials and positive helper/copy. Add regression for generated guard and actual absent/empty/nonempty request fields. No weakening to Ignore, source writes or broad grants.
