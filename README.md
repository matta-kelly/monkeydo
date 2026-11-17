# monkeydo

monkeydo keeps a node true to git. Each tick it compares a dazoo checkout with the node, changes what differs, checks
that each change took, and puts back the one that did not. Monkey see, monkey do: it does what git says.

```
monkeydo run   --checkout DIR --node mutable/<name> [--lanes files,packages,accounts,secrets,containers] [--root /] [--state /var/lib/monkeydo]
monkeydo check --checkout DIR --node mutable/<name> [--quadlet GENERATOR]
monkeydo version
```

`check` proves a node's trees and lists with no node at all; with `--quadlet`, it also proves each render (the system's
and each account's) with that generator, as the node does, so CI runs the same proof.

Exit status: 0, the node is true to git; 1, a lane refused or a change failed, named in the log; 2, monkeydo could not
run.

## How it is made

- `engine/`: the loop. A lane names each difference between git and the node as a change; the engine takes each
  change through fix, then health, and undoes the one that fails, alone. The record keeps what each lane owns, so a
  thing that leaves git is removed and nothing monkeydo did not put there is touched
- `lanes/files/`: the node's files under `/etc`, from `nodes/_shared/etc` and `nodes/<node>/etc`, the node's winning.
  Each kind of file brings itself live: `sysctl.d` with `sysctl --system` and a read-back of each value,
  `sshd_config.d` with `sshd -t` and a reload of ssh. Paths the box or another part of the node keeps are never owned
- `lanes/packages/`: the node's Debian packages, from `nodes/<node>/packages`, one name a line. It installs what is
  missing and owns only what it installed, so a package the node already had is never removed
- `lanes/accounts/`: the accounts a node's rootless apps run as, from `nodes/<node>/accounts`, one name a line. Each
  has a home, its own subuid and subgid range, and linger. An account that leaves git is no longer kept, and is left
  in place: its home holds an app's data
- `lanes/secrets/`: the node's sealed secrets, from `nodes/<node>/secrets/sealed/<name>.sops`. Each must open with the
  node's age key and be no wrong seal, or the lane changes nothing. It delivers the ciphertext, loads the value as the
  podman secret `<name>` (on stdin, never in an argument or a log), and restarts every container that claims it; one
  that does not settle puts the secret back for all its consumers. A secret that leaves git is left in place
- `lanes/containers/`: the node's Quadlets, with `$DOMAIN` from its `vars.env`: the system's, from `nodes/<node>/`
  (not `users/`), into `/etc/containers/systemd`; and each account's, from `nodes/<node>/users/<account>/`, into the
  account's `~/.config/containers/systemd`, run by its own systemd, every read and write there done as the account
  (`runuser`). It owns each folder whole. The render is proved first: no placeholder left,
  something rendered, and Podman's generator accepts it (`--dryrun`). A file that left git has its unit stopped before
  it goes; each file that moved is written, systemd reloads, and its unit restarts and must settle, or it is put back
  alone. Builds go first, then the containers that use a build that changed, then the rest; a container never started since boot is started
- `units/`: restart a unit and watch it settle: active at the end, never failed, no restart of its own on the way
- `node/`: the only code that touches the machine: files under a root, and commands
- `cmd/monkeydo/`: the command

## Build, check, release

- `just check`: gofmt, go vet, go test. CI runs the same, and main requires it
- `just release <major.minor.patch>`: checks, builds, tags, puts `monkeydo-linux-amd64.tar.gz` on the release, and
  prints the `VERSION` and `SHA256` that dazoo pins

Not built: the crate lane; the `apply` and `secrets` commands for
immutable images.
