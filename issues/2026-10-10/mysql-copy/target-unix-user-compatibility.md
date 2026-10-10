# Target template can inherit incompatible custom MySQL Unix user

Status: worker refuses unsupported UID/image combinations; target template
publication proof remains main-owned.

Logical restore creates datadir UID/GID999 with mode0700. Copying a source workload
custom runAsUser1000 unchanged would prevent target access. Worker now requires
the compatible image's named mysql UID/GID999 and source effective UID root/default
or999 before target writes. This is deliberate initial support scope, not metadata
preservation of the source filesystem.

Codex prompt: seal target subPath=mysql plus compatible image/runtime Unix user
and independent generated app/root Secret refs before publication. Add an actual
custom-UID target acceptance test before widening support; never chown source data.
