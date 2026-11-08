# Conformance of the access policy

This document uses ASD-STE100 Simplified Technical English.

The ProcurAuth repository has a conformance runner, `access-conformance`. The runner tests a service (a PEP) with the
series U of cases. Stowr runs this series in Docker. The behavior that the cases test is in [ACCESS.md](ACCESS.md).

## Run the test

The script `test/conformance/run.sh` does the whole test. It needs only Docker and `python3`.

```sh
test/conformance/run.sh
```

The script reads the ProcurAuth repository in the directory `procurauth` next to the checkout of Stowr. A checkout in
another place needs the variable `PROCURAUTH_DIR`. A git worktree under `.claude/worktrees` is such a place.

```sh
PROCURAUTH_DIR=/path/to/procurauth test/conformance/run.sh
```

| Option | Use |
|---|---|
| `--cases U01,U02,...` | Run only these cases. The check of `expected.txt` then covers only these cases. |
| `--format text` or `--format json` | The form of the report of the runner on the standard output. The default is `text`. |
| `--no-build` | Do not build the runner. Use the program of the last build. |

| Exit code | Result |
|---|---|
| 0 | The set of the failed cases is equal to the set of `expected.txt`. |
| 1 | The set of the failed cases is not equal to the set of `expected.txt`. |
| 2 | A problem of the preparation: a bad profile, a target that does not answer, no tool. |

## What the script does

1. It builds the runner with the scripts of the ProcurAuth repository, unless the option `--no-build` is set.
2. It builds Stowr as a static binary with the image `golang:1.23`.
3. It copies the test keys of ProcurAuth, the configuration and the profile to a temporary directory. It makes the fixture:
   one file of 12 bytes and one empty folder.
4. It starts Stowr in a container with no network (`--network none`) and a read-only root. The configuration is
   `test/conformance/config.hcl`: the mode `authzen` and one volume.
5. It starts the runner in the network namespace of the container of Stowr. The two programs share only the loopback address.
6. It compares the failed cases with `expected.txt` and removes the container.

The runner has three roles. It is the client. It is the edge: it signs the identity tokens with the test keys of ProcurAuth. It is
the PDP, on `127.0.0.1:8181`. Stowr never reaches a real PDP. The script removes only the container that it started.

## The files

| File | Use |
|---|---|
| `test/conformance/run.sh` | The script. |
| `test/conformance/config.hcl` | The configuration of Stowr for the test. It uses `public_keys_file` and `token_file`, and a `cache_ttl` of `0s`. |
| `test/conformance/profile.json` | The profile of the service: the service id, the token rules, the PDP, and four operations. |
| `test/conformance/expected.txt` | The failed cases that are known. |

The operations of the profile are `use_plain` (`GET /s3` of a file), `use_ceiling` (the same read with a ceiling),
`use_budget` (`PUT /s3`) and `use_multi` (`POST /api/copy`, an operation with an Other). The profile says that the service has no
answer cache that the runner must count, because `cache_ttl` is `0s`.

## Expected result

The run of the series U gives 99 cases: 74 passed, 0 failed and 25 skipped.

The runner skips these cases:

| Cases | Reason |
|---|---|
| U18, U23, U25 to U31, U85 to U99 (24 cases) | The service has no MCP endpoint. |
| U84 | The profile has no operation `use_service`: Stowr has no operation on the node of the service. |

## The file expected.txt

The file `expected.txt` lists the cases that fail and that the project knows. Write one case id for each line, then an optional
`#` and the reason. Blank lines and comment lines are allowed.

The script exits with 0 only when the set of the failed cases is equal to this list. A case that fails and is not in the list
is a failure of the test. A case in the list that passes is also a failure: remove it from the list. The list is empty now,
because no case fails.

## Continuous integration

The workflow `.github/workflows/conformance.yml` runs the script on `workflow_dispatch` and on a pull request that changes
`src/access*.go`, `src/app.go`, `src/config.go`, `src/config_keys.go`, `test/conformance/` or the workflow. The job checks out one
fixed commit of the ProcurAuth repository, and sets `PROCURAUTH_DIR` to it.

## The commit of the runner

The workflow pins one commit of the ProcurAuth repository: `7b1365206a363833ffe104b74357869636a4bb3f` (2026-11-07, the
rule of `use_ceiling` for U37). Thus a change of the runner never changes the result by itself.

A later commit adds cases. With the commit `fe9719f8949e3af2d6508a9671a415c001c678fb` (2026-11-08), the run gives 110 cases:
74 passed, 0 failed and 36 skipped. The new cases U100 to U110 are cases of the approval profile, and they need an MCP
endpoint.

To change the commit:

1. Check out the new commit of the ProcurAuth repository.
2. Run `PROCURAUTH_DIR=/path/to/procurauth test/conformance/run.sh`.
3. Update `test/conformance/expected.txt` and the numbers of "Expected result" in this document.
4. Write the full SHA of the new commit in `ref` of the workflow and in this section.
