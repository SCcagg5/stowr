# Development and validation

## Requirements

- Go 1.22 or newer;
- Docker only for the optional test stacks (SeaweedFS, MinIO and Garage) and the scratch-image test.

## Build

```sh
cd src
CGO_ENABLED=0 go build -trimpath -o stowr .
```

## Go validation

```sh
cd src
test -z "$(gofmt -l *.go)"
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /tmp/stowr-check .
```

## Configuration reference

`docs/configuration-reference.md` is generated from the table of keys in `src/config_keys.go`. After a change of the table, make it again:

```sh
cd src
go run . config-reference > ../docs/configuration-reference.md
```

The test `TestConfigurationReferenceIsCurrent` fails when the file and the table do not agree.

## Frontend

The frontend under `src/public` is plain HTML, CSS and JavaScript embedded into the binary. It has no build step and no
JavaScript toolchain: edit the files, rebuild the binary (or the test image) and reload. The object browser page
(`public/index.html`, `assets/js/app.js`) renders its DOM directly; there is no framework. The binary embeds this one
interface: no key selects an interface.

- Change a colour, a size, a radius or a duration in the tokens at the top of `assets/css/stowr.css`. The components name tokens only.
- A new icon name needs a rule in `assets/css/icons.css` and a Tabler file in `assets/icons/tabler/`.
- `go test ./...` runs `TestInterfaceIsSelfContained`. It finds an external URL, a missing asset, a missing icon file and an icon name without a rule.

### Browser tests of the interface

`test/frontend/run.sh` starts three S3 providers (SeaweedFS, MinIO and Garage) and Stowr. Stowr also shows the Docker volume `volume-data` as the volume `files`: the Playwright container mounts the same Docker volume at `/volume`, so that the tests can read the real files. It uses Docker Compose with the project name `stowr-frontend-tests`. Then it puts test objects in the buckets and runs `browser-tests.js` in the Playwright container. The tests cover the theme selector, the header, the bucket selector, the breadcrumb and the toolbar row, the bulk upload and the access behaviour on SeaweedFS. A smoke test and a test of the copy in parts run on each provider. The image of this stack copies in parts from 8 KiB up: refer to the section "Copy in parts" of [ARCHITECTURE.md](ARCHITECTURE.md). At the end, the script stops the stack.

`test/frontend/providers.js` tells which provider has versions and tags. `test/frontend/s3.js` sends signed S3 requests to the providers, for the seed and for the checks of the tests. Refer to [test/README.md](../test/README.md) for the functions of each provider. Two runs at the same time on one machine need two project names: set `FRONTEND_TEST_PROJECT`.

```sh
./test/frontend/run.sh
```

The Playwright container installs the `playwright` package one time, in a Docker volume. To use a copy that is already on disk, set `PLAYWRIGHT_NODE_MODULES` to its `node_modules` directory. To run some tests only, set `ONLY` to a part of their names.

## FUSE test of the volumes

`test/volume-fuse/run.sh` is the real test of a volume on a FUSE mount of SeaweedFS (`weed mount`). It starts the
SeaweedFS of the tests and one container with `weed mount`, the Go toolchain and Stowr, under the project name
`s3-volmount`. Only this container gets `/dev/fuse`, `SYS_ADMIN` and no AppArmor profile. The script runs the Go tests
of the volumes in a folder of the mount, then a browser smoke test. At the end, it removes the stack.

```sh
./test/volume-fuse/run.sh
```

The Go tests of the volumes take the flag `-volume-dir`: with it, each test makes its folders in a new folder of that
folder, not in a temporary folder of the system. Use it to run the tests on an other mount:

```sh
cd src
go test -count=1 -run TestVolume . -args -volume-dir=/mnt/nfs/stowr-tests
```

## Conformance test of the access policy

`test/conformance/run.sh` runs the ProcurAuth conformance runner against Stowr in Docker. It needs the ProcurAuth repository:
set `PROCURAUTH_DIR` when the repository is not next to the checkout of Stowr. The workflow `.github/workflows/conformance.yml`
runs it on a pull request that changes the access code. Refer to [CONFORMANCE.md](CONFORMANCE.md).

```sh
./test/conformance/run.sh
```

The Go tests of the access policy are in the files `src/access*_test.go`. They use a fake PDP in a test HTTP server. They need
no network and no Docker.

## Filesystem-write check

The program keeps no state on the local file system. Only the files of the volume backend (`src/volume_*.go`) write
to the local file system: they write the files of a `volume` block (refer to the section "Volumes" of
[ARCHITECTURE.md](ARCHITECTURE.md)). Each other production Go file must not call a write API of the file system. The CI
does this check:

```sh
if grep -R --include='*.go' --exclude='*_test.go' --exclude='volume_*.go' \
  -E 'os\.(Create|Mkdir|MkdirAll|WriteFile|OpenFile|Remove|RemoveAll|Rename|Link|Symlink|Truncate|Chmod|Chown|Lchown|Chtimes)\b|CreateTemp|MkdirTemp|syscall\.(Open|Creat|Mkdir|Rmdir|Unlink|Rename|Link|Symlink|Truncate)\b' src; then
  exit 1
fi
```

Put new code that writes the files of a volume in a file named `volume_*.go`. Do not put other code in these files.
Tests may create fixtures inside their temporary test directories.

## Shell syntax

```sh
for file in test/*.sh test/seaweedfs/scripts/*.sh test/minio/scripts/*.sh test/garage/scripts/*.sh test/frontend/*.sh test/volume-fuse/*.sh test/volume-fuse/scripts/*.sh; do
  [ -e "$file" ] || continue
  sh -n "$file"
done
```

## PDF.js assets

The repository expects the official `pdfjs-dist` 4.10.38 ESM module and worker at:

```text
src/public/assets/vendor/pdfjs/4.10.38/pdf.min.mjs
src/public/assets/vendor/pdfjs/4.10.38/pdf.worker.min.mjs
```

Keep the Apache-2.0 license notice in the same directory. The application does not fetch these assets at startup or at runtime.

## Optional SeaweedFS stack

```sh
cd test
docker compose up --build
```

The browser container is read-only and has no application data volume. SeaweedFS keeps its own provider storage as required by the test service. A provider can return HTTP 501 for version listing. The browser must start normally and omit version controls for those buckets.

## Scratch runtime

```sh
./test/scratch-runtime.sh
```

The test builds the scratch image, checks the numeric user and static binary identity, starts the container with `--read-only`, and runs the built-in health check.

## Release validation

CI checks formatting, race tests, vet, all JavaScript contracts, shell syntax, absence of process-environment configuration (only `src/config_env.go` may read a variable that an `_env` attribute names), absence of local runtime write APIs, absence of external frontend assets, static linking, and the read-only scratch runtime.

Before distributing an archive, extract that exact archive into a clean directory and repeat the full validation suite there.
