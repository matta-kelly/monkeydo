# monkeydo

monkeydo keeps a node true to git. Each tick it compares a dazoo checkout with the node, changes what differs, checks
that each change took, and puts back the one that did not. Monkey see, monkey do: it does what git says.

```
monkeydo run   --checkout DIR --node mutable/<name> [--root /] [--state /var/lib/monkeydo]
monkeydo check --checkout DIR --node mutable/<name>
monkeydo version
```

Exit status: 0, the node is true to git; 1, a lane refused or a change failed, named in the log; 2, monkeydo could not
run.

## How it is made

- `engine/`: the loop. A lane names each difference between git and the node as a change; the engine takes each
  change through fix, then health, and undoes the one that fails, alone. The record keeps what each lane owns, so a
  thing that leaves git is removed and nothing monkeydo did not put there is touched
- `lanes/files/`: the node's files under `/etc`, from `nodes/_shared/etc` and `nodes/<node>/etc`, the node's winning.
  Each kind of file brings itself live: `sysctl.d` with `sysctl --system` and a read-back of each value,
  `sshd_config.d` with `sshd -t` and a reload of ssh. Paths the box or another part of the node keeps are never owned
- `node/`: the only code that touches the machine: files under a root, and commands
- `cmd/monkeydo/`: the command

## Build, check, release

- `just check`: gofmt, go vet, go test. CI runs the same, and main requires it
- `just release <major.minor.patch>`: checks, builds, tags, puts `monkeydo-linux-amd64.tar.gz` on the release, and
  prints the `VERSION` and `SHA256` that dazoo pins

Not built: the packages, accounts, secrets, containers and crate lanes; the `apply` and `secrets` commands for
immutable images.
