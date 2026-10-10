# Publication gate compatibility identifier casing

Status: fixed locally. New mixed-case GitHub owner literals violated the required
diff-based brand guard. New compatibility identifiers now use lowercase envplane.
Repository comparisons fold case only and still require an exact owner/repo;
leading whitespace, suffixes, foreign owners and missing identities remain denied.

Regression: the existing 44 gate tests now also cover mixed-case context/payload
and run repository identities, plus exact-repository look-alike rejection.

Codex prompt: keep compatibility repository identifiers lowercase when changing
the gate, preserve case-insensitive exact comparisons, and run gate tests plus
scripts/check-brand.sh --diff-base origin/main before handing off commits.
