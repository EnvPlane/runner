# Online DML changes mutable table AUTO_INCREMENT counter

Status: resolved locally; real AUTO_INCREMENT writer fixture plus regression tests.

Raw CREATE TABLE/dump bytes can differ for an equivalent restored snapshot while
DML advances a table's counter. Only the numeric table option outside all quotes
and column parentheses is normalized for schema proof. Raw source byte count/SHA
remain transfer audit; schema/ordered-row proofs define restore equality. Literal,
column, index and constraint mutations continue to refuse. Restore now uses binary
mode, disables LOCAL INFILE and forwards only complete validated statements.

Codex prompt: preserve these exact narrow comparison and SQL framing boundaries.
Add real online DML tests for future MySQL client/schema profiles; never normalize
arbitrary identifiers, literals, type/constraint definitions or row content.
