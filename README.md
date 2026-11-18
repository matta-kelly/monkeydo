# monkeydo

monkeydo keeps a Linux host matching a git repository. It reads a node's declaration (its files, packages, accounts,
systemd units, secrets and Podman Quadlets), compares it with the host, and changes only what differs. Each change is checked after it
is applied, and a change that fails its check is rolled back on its own.

Monkey see, monkey do: it does what git says.

## How it works

A run goes through each **lane**, one per kind of thing a host has. For every difference between git and the host, the
engine takes the same steps:

1. **Prove**: check the change before touching the host (for example `sshd -t`, `quadlet --dryrun`, or decrypting a
   secret).
2. **Apply**: make the change atomically.
3. **Bring live**: reload or restart what reads it.
4. **Check health**: confirm it took, such as a sysctl value reading back or a unit staying active with no restarts.
5. **Roll back**: if the check fails, restore that one change. The rest of the run is kept.

A run with nothing to change does nothing. monkeydo keeps a record of what it owns, so a thing removed from git is
removed from the host, and nothing it did not create is touched.

## Lanes

| Lane | Source in the repo | What it keeps |
| --- | --- | --- |
| `files` | `nodes/_shared/etc/`, `nodes/<node>/etc/` | Files under `/etc`, each with the reload its kind needs. A changed unit file reloads systemd and restarts the unit if it runs |
| `packages` | `nodes/<node>/packages` | Debian packages. Only packages monkeydo installed are ever removed |
| `accounts` | `nodes/<node>/accounts` | Users with a home, subuid and subgid ranges, and linger. Never deleted |
| `units` | `nodes/<node>/units` | systemd services and timers that run outside a container, enabled and running. Only units monkeydo started are ever stopped |
| `secrets` | `nodes/<node>/secrets/sealed/*.sops`, `nodes/<node>/secrets/files` | SOPS-sealed values loaded as Podman secrets, their containers restarted on a change; or, where `files` lists them, written as files, mode 0600 |
| `containers` | `nodes/<node>/`, `nodes/<node>/users/<account>/` | Quadlets for the system and for each account, in its own systemd |

A unit's file comes from the `files` lane (`etc/systemd/system/`) or from a package. List a timer, not the oneshot
service it fires: a oneshot ends inactive, so it never reads as running.

A secret file is for a reader that takes a credential only from a file, such as Podman's registry login. Each line of
`secrets/files` is a sealed name and a path: absolute, written by root, or `~<account>/<path>`, written as that account.

Secret values are passed on stdin only, never in arguments or logs. In an account's home, monkeydo reads and writes only
as that account.

## Usage

```
monkeydo run   --checkout DIR --node mutable/<name> [--lanes files,packages,accounts,units,secrets,containers]
monkeydo check --checkout DIR --node mutable/<name> [--quadlet GENERATOR]
monkeydo version
```

- `run` applies the lanes to the host. Use `--root` and `--state` to point it at another root and record folder.
- `check` proves a declaration without a host, for CI. With `--quadlet`, it also validates every Quadlet render with
  that generator.

| Exit status | Meaning |
| --- | --- |
| 0 | The host matches git |
| 1 | A lane refused its input, or a change failed and was rolled back. The log names it |
| 2 | monkeydo could not run (usage, another run holding the lock, or the record) |

## Development

```
just check                  # gofmt, go vet, go test (CI runs the same; main requires it)
just release <x.y.z>        # check, build, tag, publish monkeydo-linux-amd64.tar.gz, print VERSION and SHA256
```

Code layout: `engine/` (the run and its record), `lanes/` (one package per lane), `units/` (restart and health checks
for systemd units), `node/` (the only code that touches the host), `cmd/monkeydo/` (the command).

## Status

Not built yet: commands for immutable images.
