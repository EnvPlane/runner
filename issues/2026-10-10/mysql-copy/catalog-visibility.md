# Filtered MySQL catalogs cannot prove object absence

Status: resolved fail-closed; operator credential provisioning remains external.

A SELECT-only dump principal cannot establish trigger/event/routine absence from
zero information_schema counts. Driver now requires explicit direct catalog
visibility grants on the approved separate admin, and compares full and dump
principal table inventories before target creation. Unsupported objects refuse.

Codex prompt: maintain negative tests for blind, role-only and wrong-schema catalog
authority. Add reviewed role-expansion/catalog privilege support only with real
MySQL evidence. Never silently skip views, stored code or hidden tables; no live
source grant creation by runner. Empty DB support is not currently implemented.
