# Temporary target reused app password for root

Status: resolved locally; main's generated Secret/adapter owns integration.

Target.RootPasswordKey is now required and distinct from the app key. Exact
approved target Secret UID and hashes bind independently generated values;
root/app/source password equality refuses before PVC creation. Random entrypoint
root generation is forbidden because official images log that password.

Codex prompt: preserve independent MYSQL_ROOT_PASSWORD and app password generation
in the preceding Secret gate and its UID/digest proof. Do not copy source accounts
or credentials, overwrite generated Secrets, or log/put passwords in argv.
