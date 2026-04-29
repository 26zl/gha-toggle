# gha-toggle

Bulk-disable and re-enable GitHub Actions workflows across all your repos — useful when you run out of free Actions minutes and want to stop everything until the quota resets.

## Requirements

- Go 1.23+
- [`gh`](https://cli.github.com/) installed and logged in (`gh auth login`)

## Install

```bash
go build -o gha-toggle .
mv gha-toggle ~/.local/bin/
```

## Usage

```bash
gha-toggle status                  # show billing + workflow counts
gha-toggle list                    # list all workflows and their state

gha-toggle disable-all --dry-run   # preview
gha-toggle disable-all             # disable everything, save state
gha-toggle enable-all              # re-enable from saved state

gha-toggle disable-repo owner/repo # one repo at a time
gha-toggle enable-repo  owner/repo
```

State is saved to `~/.gha-toggle/enabled-workflows.json` and used by `enable-all` to flip exactly the same workflows back on.

## Flags

| Flag                  | Description                                        |
| --------------------- | -------------------------------------------------- |
| `--dry-run`           | Print actions without calling the API              |
| `--owner <login>`     | Only touch repos owned by this user/org            |
| `--include-forks`     | Include forked repos (skipped by default)          |
| `--include-dynamic`   | Include `dynamic/` workflows (always 422; skipped) |
| `--concurrency <n>`   | Parallel API calls (default: 8)                    |
| `--json`              | JSON output for `list` and `status`                |
| `--state-file <path>` | Use a custom state file                            |

## Tests

```bash
go test ./...
```
