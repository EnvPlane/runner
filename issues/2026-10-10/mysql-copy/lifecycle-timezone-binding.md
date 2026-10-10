# Equivalent lifecycle timestamps changed local receipt digest

Status: resolved locally; canonical-UTC regression added.

The domain lifecycle digest is UTC-canonical, but local worker JSON previously
retained equivalent timezone offsets. A rebuilt adapter could spuriously reject
a completed target receipt. Local plan hashing now canonicalizes the timestamp
to UTC without changing the authoritative domain/Secret-gate digest references.

Codex prompt: preserve exact lifecycle instant binding across retries and timezone
round trips. Never drop EnvironmentCreatedAt or adopt a target from another plan.
