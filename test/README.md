# Integration test stack

This directory contains an isolated SeaweedFS object-storage service and the browser configuration used by smoke tests.

The SeaweedFS service is one process: it has the master, the volume server, the filer and the S3 API. `seaweedfs/Dockerfile` downloads the release 4.41 and checks its SHA-256 checksum. This version is the one of the Home repository. `seaweedfs/scripts/entrypoint.sh` creates the identity and the buckets `default` and `archive` from `config.hcl`. It also creates the bucket `locked` with Object Lock (`s3.bucket.create -withLock`), and then writes a ready file for the health check.

`config.hcl` defines one S3 authentication shared by two buckets. Each bucket has its own permission ceiling and navigation scan limit.

The browser container:

- uses the HCL file read-only;
- runs with a read-only root filesystem;
- has no application data volume;
- retains jobs and upload sessions only in process memory;
- must start when a provider returns HTTP 501 for version listing.

Start the stack:

```sh
docker compose up --build
```

Open:

```text
http://localhost:8080/
```

To expose the application under `/stowr/`, configure the reverse proxy to remove that prefix before forwarding. The frontend resolves all URLs relative to the public document path and requires no HCL path setting.

The fixed credentials in `config.hcl` and `secrets/` belong only to the isolated SeaweedFS service. Do not reuse them outside this test environment. The secret key is in `secrets/s3_secret_access_key`, because the browser refuses a secret in clear text in the HCL file. The compose file mounts `secrets/` at `/config/secrets` in both containers.

## Browser test stack: three providers

`frontend/compose.yaml` starts three S3 providers next to Stowr. `frontend/run.sh` starts the stack, puts the test objects and runs the browser tests. Refer to [docs/DEVELOPMENT.md](../docs/DEVELOPMENT.md#browser-tests-of-the-interface).

| Provider | Image | Instance | Endpoint |
| --- | --- | --- | --- |
| SeaweedFS 4.41 | `seaweedfs/Dockerfile`: the release archive, with its SHA-256 checksum | `seaweedfs-main`, `seaweedfs-archive`, `seaweedfs-locked`, `seaweedfs-encrypted` | `http://seaweedfs:8333` |
| MinIO RELEASE.2025-10-15T17-29-55Z | `minio/Dockerfile`: a build from the Git tag, with a check of its commit | `minio-main`, `minio-locked`, `minio-encrypted` | `http://minio:9000` |
| Garage v2.4.1 | `garage/Dockerfile`: the binary of `dxflrs/garage:v2.4.1`, pinned by its digest | `garage-main`, `garage-encrypted` | `http://garage:3900` |

The MinIO project does not publish a server image or a server binary now, and its repository is archived. Thus `minio/Dockerfile` builds the last release from source. The build takes about one minute, one time. Docker keeps the image in its cache.

Each provider reads the first `access_key_id` and the first `secret_access_key_file` of `frontend/config.hcl`. Thus the three `auth` blocks use the same key. Garage needs this format: `GK` and 24 hexadecimal characters for the key ID, 64 hexadecimal characters for the secret. Each entrypoint creates the bucket `default` and then writes a ready file. The entrypoints of SeaweedFS and MinIO also create the bucket `locked` with Object Lock. A bucket gets Object Lock only at its creation (`x-amz-bucket-object-lock-enabled: true`). Object Lock turns on its versioning. Garage cannot make a bucket with Object Lock. The instances `seaweedfs-locked` and `minio-locked` of `frontend/config.hcl` use these buckets, and `frontend/providers.js` lists them in `LOCKED`. The health check of each provider reads this file, so the browser starts after the buckets exist. The Garage region is `garage`.

The seed (`frontend/seed.js`) enables the versioning of the bucket `default` on SeaweedFS and MinIO. It puts the same objects in `smoke/` on each provider: one file with tags where the provider has tags, and one file with two versions. On SeaweedFS, the seed also puts the data of the other tests: files with several versions in `versions/`, tags, a lifecycle rule, and a JSON file, a CSV file and a binary file for the comparison. The other browser tests use SeaweedFS only.

The image of Stowr in this stack copies in parts from 8 KiB up, in parts of 5 MiB (the build arguments `MULTIPART_COPY_THRESHOLD` and `MULTIPART_COPY_PART_SIZE` of `browser/Dockerfile`). The seed puts `copy/source.bin` (11 MiB, three parts) with standard headers, a user metadata and tags on each provider. On SeaweedFS and MinIO, it also puts `copy/versioned.bin` with two versions. The tests of the copy in parts copy and rename `source.bin` through the page and compare the SHA-256, the headers and the tags. They also restore the older version of `versioned.bin`.

The seed also puts the objects of the metadata tests in `meta/` on each provider: `small.txt` (under the threshold of the copy in parts), `large.bin` (20000 bytes, above it) and `conflict.txt`, each with `Cache-Control: max-age=60` and the user metadata `owner`. `small.txt` and `large.bin` have tags where the provider has them. `versioned.txt` has two versions on SeaweedFS and MinIO. The metadata tests edit these objects through the details on each provider. They check each sentence that the form shows before the save against the result: the date, the ETag, the versions and the tags. They also check that the form shows and sends the stored `Cache-Control`, not the value of the gateway (`private, no-store, max-age=60`). Refer to the section "Edit of the metadata" of [docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md).

The seed also puts four incomplete multipart uploads in `mpu/` on each provider, with S3 requests: `one-part.bin` (one part of 1 KiB), `no-part.bin`, and two uploads of `twice.bin`. The folder has `readme.txt`, so the page can open it. The test of each provider opens the window "Incomplete uploads" of the folder. It checks the parts, the age ("unknown" on SeaweedFS) and "Abort everything older than 1 day", which aborts nothing. It then aborts one upload of `twice.bin` with "Abort selected" and `one-part.bin` with the button of its row. The test aborts the last two uploads through the API. A second test starts a resumable upload through the API and checks that the window and the server do not abort it. Refer to the section "Cleanup of incomplete multipart uploads" of [docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md).

The tests of the lifecycle editor write the rules of the bucket `default` through the S3 API of each provider first (`frontend/s3.js`). At the end, each test writes the rule of the seed again on SeaweedFS and deletes the rules on MinIO and Garage: the tests of the tags read the rule of SeaweedFS. The editor is the part "Lifecycle rules" of the information page (`?info#info-lifecycle`): a form at the left and the rules at the right, with no window. The test of each provider adds a rule with the form, with the confirmation of the deletion in the page. Add stays grey until the rule has an ID and one valid action. The test checks that the rule with a date stays byte for byte, that the form offers only the fields that the provider keeps, and that the hourglass of the selector and the chip of the card Operations count the rules that are on. A second test opens the editor from the window "Incomplete uploads", with the rule that aborts the uploads in the form, where the provider keeps it. Other tests check Edit, Update and Revert, the question of Delete in the row, a change of the rules after the page read them, the changes of the provider after a save, a read-only editor, and an instance without rules.

The tests of Object Lock use the instances `seaweedfs-locked` and `minio-locked`. Each test makes its own object with a unique key. The first test makes two versions, because the details show the version picker only for a file with two versions or more. It sets a legal hold on the current version through the details, and tries to delete this version with the Delete button of the picker. It reads the message that names the lock, removes the hold and deletes the version. A second test extends a Governance retention, and shortens it with the confirmation of the bypass. A third test checks that the page and the server refuse to shorten a Compliance retention. The retentions end after a few seconds, and each test then deletes its version: a run leaves no locked version. `run.sh` also removes the volumes of the stack after each run. A test checks that the section is hidden on `garage-main` and on the buckets without Object Lock. On each locked bucket, a test deletes a file and a folder through the page. The confirmation and a warning say that the locked version stays, and the version stays behind a delete marker. A bucket without Object Lock has no such sentence. Refer to the section "Object Lock" of [docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md).

SeaweedFS and MinIO really encrypt with SSE-S3 in this stack. The entrypoint of SeaweedFS gives the S3 server a random key encryption key (`WEED_S3_SSE_KEK`, 32 bytes in hexadecimal). The entrypoint of MinIO gives it a random KMS key (`MINIO_KMS_SECRET_KEY`, `stowr-test:` and 32 bytes in base64). Each start makes new keys. They stay in the environment of the process: no file, no log, no output. The data of the stack does not outlive its containers. Both entrypoints also create the bucket `encrypted`. The instances `seaweedfs-encrypted` and `minio-encrypted` use it with `encryption = "sse-s3"`, and `frontend/providers.js` lists them in `ENCRYPTED`. The instance `garage-encrypted` uses the bucket `default` of Garage with `encryption = "sse-s3"`. Two instances can use the same bucket. The seed enables the versioning of the buckets `encrypted`. It puts `enc/customer-key.txt` with a key of the client (SSE-C) on SeaweedFS and Garage, with a random key that only the seed process knows. It also puts `enc/kms.txt` with SSE-KMS on MinIO.

The tests of the encryption upload a file through the page to each encrypted instance, in one request and in parts. They copy, rename, move, copy in parts, edit the metadata (also in parts) and restore an older version. Each source is not encrypted. After each operation, the HEAD request on the provider says `AES256`, and the details say "SSE-S3 (AES256)". On MinIO, a metadata edit and a restore keep `aws:kms` and its key. Other tests put a default encryption on the bucket `default` of SeaweedFS and MinIO, check the row "Bucket default", and remove it. They also check the details of the SSE-KMS and SSE-C objects. On Garage, an upload to `garage-encrypted` works, the object has no encryption, and the details show no encryption section.

Other tests of the encryption check the preview, the link to the original and the downloads of the SSE-C object: they give the message of the key of the client. A HEAD answer `400` of an other object stays an error. The bucket selector shows a key for `seaweedfs-encrypted` and `minio-encrypted`, a key in the colour of a warning for `garage-encrypted`, and no key for the other instances. The listing shows a key next to an encrypted file by default on each instance (SSE-S3, SSE-C, SSE-KMS). The setting goes off for one bucket and stays in the browser. A test with 120 files counts the HEAD requests: 4 at the same time, 100 files at most, one request for each file. A test writes a version without encryption and a version with SSE-S3 of one key, and checks the marks of the preview and the details of each version. Refer to the section "Encryption" of [docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md).

On `seaweedfs-locked` and `minio-locked`, a test writes files with a legal hold, a Governance retention, a Compliance retention, a past retention, no lock, and SSE-S3 with a lock. The listing and the preview show the padlock with the right tooltip, and the key and the padlock for the last file. The bucket selector shows the padlock of the bucket. The setting stops the marks. The test counts one HEAD request for each file and no request of `/api/object-lock`. The Compliance retention has 45 seconds, and the test waits for its end before it deletes the versions.

Each provider gives the usage and the quota of its buckets in its admin API, and each `auth` block of `frontend/config.hcl` has a `usage` block. The entrypoints give the bucket `default` of each provider a quota of 20 GiB, a large value that no test reaches. Garage also gets a quota of 100000 objects. The Garage entrypoint sets it with `garage bucket set-quotas`, SeaweedFS with `s3.bucket.quota` in `weed shell` (not enforced), and MinIO with `PUT /minio/admin/v3/set-bucket-quota` (a hard quota). The admin API of Garage listens on port 3903 of the network of the stack. Its admin token is a test value in `secrets/garage_admin_token`: the entrypoint and the `token_file` of the browser read the same file. The browser reads the master (port 9333) and the filer (port 8888) of SeaweedFS, and the admin API of MinIO on its S3 endpoint with the root key. The instance `seaweedfs-archive` has `quota_bytes = 10737418240` (10 GiB): its bucket has no quota at the provider. The other buckets have no quota.

The tests of the usage check the usage in the bucket selector on each provider: the amount, the percent, the bar, the lines of the tooltip of the trigger and the `~` of SeaweedFS and MinIO. A click on the usage opens the information page, and the test reads its gauges there: what the bucket holds, the quota of 20 GB and the storage of 4 GB. The tests also check a bucket without a quota, the limit of `quota_bytes`, an instance without usage (no request of `/api/usage`), and a failed read (no number, the reason for a screen reader and on the information page, no notice). On Garage and SeaweedFS, a test uploads 3 MiB and waits until the number grows. MinIO counts in a scan of about one minute, so its growth is not tested. Other tests check the tooltips of the bucket selector, the pills of the details and the sections that close.

The tests of the state icons of the bucket selector check the seven icons of a bucket (read, write, delete, encryption, Object Lock, versioning, lifecycle rules) and the three icons of a volume, in the trigger and in each entry. Each icon is green, orange or grey: the key of `garage-encrypted` and a suspended versioning are orange, a denied right is grey, and no icon is red. Each entry of the list has its states from `/api/instances`: the seed turns on versioning and adds a rule after the start, so the test waits until the server reads them again (60 seconds at most). The page reads the usage of the buckets 4 at a time, with "…" while a read waits. The icons and the usage are one link to the information page, with no tooltip: a test opens it with the mouse, Enter and Space, and from the usage of the trigger and of an entry. A test compares the markup of the selector on the listing and on the information page, and the static trigger of one visible bucket on the two pages.

The tests of the layout of the header and of the selector measure them at 14 widths from 320 to 1920 px, with a fine and a coarse pointer. The header is one row of 45 px, with a tolerance of 1 px (57 px on a coarse pointer). Its controls have one vertical centre within 2 px, and the page has no horizontal scroll. The trigger is one row of 28 px (44 px) on five instances and the volume, on a long name with every state and a full bucket, on one visible bucket and on a failed read of the usage; the seven icons stay at every width. A test checks the order in which a narrow selector drops the parts of the usage, and the states in the tooltip. Each bucket and each host of the list is one line, a root prefix follows its name, the state icons make one column, and the list has the edges of the trigger. A host with one entry (the volume) has no row. The bar of the preview page is one row at each width; a comparison keeps one row from 761 px. At 1920, 1440 and 1000 px, the first element of the header is 12 px from the left edge and the theme button 12 px from the right edge (10 px at 820 and 390 px), on the list page, on the preview page and in a comparison.

The tests of a full bucket answer `/api/usage` with `page.route`: 91 %, 100 % and 50 % of the limit, and a bucket without a limit of bytes. They check the banner, its `role="alert"`, its texts, the marks of the selector, the mark of another full bucket and the tooltip of the upload button. The real usage of the test stack gives no warning. A test of the details checks that Storage object and Tags are side by side at 1440 px, and one under the other at 390 px.

A test opens each menu of the interface in the two themes: the theme menu, the list of the buckets, the menus Actions and settings, a menu of a row, the suggestions of the metadata editor, and on the preview page the theme menu, the menu of the actions and the version picker. The background of each menu has an alpha of 1, the menu has an opacity of 1 in the first frame of its animation, and no element of the page is over it.

Other tests check the settings menu: no part is wider than the menu at 390 and 1280 px. They check the columns that are on by default on `seaweedfs-main` and `garage-main`, and the saved choices. They also check the listing without totals.

Other tests check the information page (`?info`): one page with no tabs and no head, three columns at 1440 px and one on a phone, the permissions on one line, the link Browse in the header, and no endpoint, token or path. The page of a volume has no part of the lifecycle rules, and a bucket that does not exist gives a 404. A test of the scroll checks on the listing, the information page, the preview and the 404 page that the header never scrolls: only the element under it scrolls, from the header to the right edge, and the document stays at the top.

The browser tests start each page with all the sections of the details open (the choice that the browser keeps in `stowr.details.sections`). The tests of the sections start with the default: "Storage object" and "Tags" open, the other sections closed.

The smoke test of each provider lists `smoke/`, uploads a file with its SHA-256 checksum, downloads it and deletes it. It checks the versions and the tags where the provider has them. Where the provider does not have them, it checks that the page hides them. `frontend/providers.js` has the flags of each provider.

## Capacity of the storage in the browser test stack

The data of each provider of the browser test stack is on a tmpfs of 4 GiB (`/data` of SeaweedFS and MinIO,
`/var/lib/garage` of Garage), and the Docker volume of the volume `files` is a tmpfs of 1 GiB. Thus the capacity of the
storage behind each bucket (`storage` of `/api/usage`) is the same on each test host, and far from full. The tests of
the usage check the gauges of the information page on each provider: what the bucket holds, the quota of 20 GiB, and
the storage of 4 GiB. The storage of 4 GiB
holds all the buckets of a provider, so the chip of the selector shows the storage, or the quota for an equal part
(Garage compresses its data). Other tests check a phone (no usage in the trigger, the link to the information page), an
instance without a `usage` block, and the banner of a storage at 91 % with a quota at 50 % (and the
reverse) with a faked `/api/usage`. The test of the volume compares the size that the server counts with the files of
`/volume`.

## Volume of the browser test stack

The browser test stack also has the volume `files`: the Docker volume `volume-data` at `/data/volume` of the browser
container. The root file system of the browser container stays read only. The one-shot service `volume-init` gives the
Docker volume to the user of Stowr (65532) before Stowr starts, because the start checks that the process can
write in the folder. `frontend/run.sh` also mounts the Docker volume at `/volume` of the Playwright container.
`frontend/providers.js` lists the volume in `VOLUMES`.

The seed writes the files of the volume through the API: `docs/` with a nested folder, `data/` with a CSV file and a
JSON file, and `images/` with an image. It also writes, through `/volume`, the files that the API cannot make: a hidden
file, an empty folder, three symbolic links in `links/` (to a folder of the volume, to a file of the volume, and to
`/etc`, which leaves the volume), and a temporary file of an upload with a date three days ago in `uploads/`.

The tests of the volume check the address `/-/volume/files/`, the real and empty folders, the hidden file, the nested
folders and the hidden functions of S3 (tags, versions, lifecycle rules, Object Lock, encryption, edit of the
metadata). They check the links (the unreadable link has no action, and the API refuses it with `403`), and `..` in a
key (`400`). They make a folder with "New folder", upload a small file with its SHA-256, copy, rename (the same inode)
and delete a file, and copy, rename and delete a folder with an empty folder. An upload of 40 MiB loses its second
chunk and resumes: the test sees the temporary file in the folder during the upload, and no temporary file at the end.
It reads a range and the weak ETag. Other tests check the previews, the Insights, the prefix search, the window
"Incomplete uploads" (the date of the last write, no parts, the abort by age), and the usage of the file system with
the banner of a full volume.

## FUSE test of the volumes

`volume-fuse/run.sh` is the real test of a volume on a FUSE mount of SeaweedFS. Refer to
[docs/DEVELOPMENT.md](../docs/DEVELOPMENT.md#fuse-test-of-the-volumes).

- The service `seaweedfs` is the SeaweedFS 4.41 of the browser tests, with its configuration.
- The service `mount` (`volume-fuse/Dockerfile`) has the same pinned `weed` binary, FUSE 3, the Go toolchain and
  Stowr. Only this container has `/dev/fuse`, `SYS_ADMIN` and `apparmor:unconfined`. Its entrypoint runs `weed
  mount -filer=seaweedfs:8888 -filer.path=/ -dir=/mnt/sw -volumeServerAccess=filerProxy`. The volume servers of the
  service `seaweedfs` listen on their own address only, so the data goes through the filer. Stowr then starts
  with the volume `sw` on `/mnt/sw/stowr` (`volume-fuse/config.hcl`).
- `run.sh` runs the Go tests of the volumes on the mount (`go test -run TestVolume -args -volume-dir=/mnt/sw/stowr-go`),
  then `volume-fuse/smoke.js` in the Playwright container: the list, a folder, an upload with SHA-256, a rename, a
  copy, a range read, a resumed upload of 40 MiB, and the copy, the move and the delete of a folder.

These facts come from the run of 2026-10-21 on a Debian 13 host (Linux 6.12, Docker 29.9, `/dev/fuse` present):

- FUSE works in Docker on this host with these three options. The mount is `fuse.seaweedfs` with `allow_other`.
- The 22 Go tests of the volumes pass on the mount, also the symbolic links, the rename that keeps the inode, the
  dates of the files (`os.Chtimes`), `O_EXCL`, `fsync` and the hard links. The test of a folder that the process cannot
  write is skipped, because the process is root.
- The six steps of the browser smoke test pass. The writes of Stowr are visible at once through the mount.
- The count of the folder of the volume works on the mount: after the smoke uploads, the volume holds 41943084 bytes
  in 3 files, as the browser smoke test expects. The page showed "40 MB, 3 files" and the storage with `~`.
- `statfs` of the mount gives the capacity of the volume servers: 30 volumes of 64 MiB (2013265920 bytes). One run
  gave a size of 0 after the delete of a folder: the server then gives `storageError` ("a size of 0") and keeps what the
  volume holds. The used
  bytes follow with a delay: two runs gave 0 bytes after the first writes, and 0 or 256512 bytes after an upload of
  40 MiB and its delete. The
  server finds the type FUSE in the answer of `statfs`: the usage of the volume is approximate, and the page shows it
  with `~` and the reason.

When a host refuses FUSE in Docker (no `/dev/fuse`, or a security profile that refuses the mount), `run.sh` stops with
the log of `weed mount` and a message. The tests of the browser test stack then cover a volume on a plain Docker volume.

## Functions of each provider

These facts come from S3 requests to the three providers of the browser test stack, with a check of the effect by a second request. "Yes" means that the request works and has its effect. "Dropped" means that the provider answers 200 but does not keep the setting. Other cells give the HTTP status and the S3 error code.

| Function | SeaweedFS 4.41 | MinIO 2025-10-15 | Garage v2.4.1 |
| --- | --- | --- | --- |
| Object tags (Get, Put, Delete) | Yes | Yes | 501 NotImplemented |
| Header `x-amz-tagging` on PutObject | Yes | Yes | Dropped |
| Versioning (PutBucketVersioning, ListObjectVersions) | Yes | Yes | 501 NotImplemented. GetBucketVersioning gives 200 with no status |
| GET or DELETE with `versionId` | Yes | Yes | The provider ignores `versionId`. A DELETE with any `versionId` deletes the object |
| DeleteObjects (batch delete) | Yes | Yes. A request with no checksum header gives 400 MissingContentMD5 | Yes |
| `x-amz-checksum-sha256` on PutObject | Yes. A wrong value gives 400 BadDigest | Yes. A wrong value gives 400 XAmzContentChecksumMismatch | Yes. A wrong value gives 400 InvalidDigest |
| `x-amz-checksum-sha256` on UploadPart | Yes. ListParts does not show the part checksum | Yes | Yes |
| Composite checksum of a multipart object (HEAD) | `<digest>-<parts>` | `<digest>-<parts>` | `<digest>`, with no part count |
| ListMultipartUploads | Yes. No `Initiated` date. Only `NextUploadIdMarker` for the next page | Partial. A prefix that is not a full key gives an empty list. `max-uploads` has no effect | Yes |
| ListMultipartUploads: order and pages, with three uploads of one key and `max-uploads=2` | Order of the upload ids. The pages continue with `upload-id-marker` only, and each upload comes one time | One page with all the uploads, in the order of the keys | Order of the keys. The pages continue with `key-marker` and `upload-id-marker`, and each upload comes one time |
| ListParts, AbortMultipartUpload | Yes. ListParts of an aborted upload gives 200 with no part | Yes | Yes |
| ListParts with `max-parts=1` of an upload with two parts | One part, `IsTruncated` true | One part, `IsTruncated` true | One part, `IsTruncated` true |
| ListParts or AbortMultipartUpload of an unknown upload id | 404 NoSuchUpload | ListParts: 404 NoSuchUpload. Abort: 404 NoSuchUpload, but 204 for the id of an upload of another key (the upload stays) | 404 NoSuchUpload |
| UploadPartCopy with `x-amz-copy-source-range` | Yes | Yes | Yes |
| UploadPartCopy or CopyObject of an older version | Yes | Yes | 404 NoSuchKey |
| `x-amz-copy-source-if-match` on UploadPartCopy, with the ETag of the source | Yes | Yes | Yes |
| Header `x-amz-tagging` on CreateMultipartUpload | Yes | Yes | No tags (not tested) |
| ETag of a copy in 3 parts | `<digest>-3` | `<digest>-3` | `<digest>-3` |
| CopyObject onto itself with `MetadataDirective=REPLACE` | Yes. The tags stay, the date changes, a new version comes | Yes. The tags stay, the date changes, a new version comes | Yes. The date changes |
| ETag after CopyObject REPLACE of an object in one part | The ETag stays | The ETag stays | The ETag stays |
| ETag after CopyObject REPLACE of a multipart object | The ETag stays (`<digest>-<parts>`) | The ETag changes: it becomes the MD5 of the object | The ETag stays (`<digest>-<parts>`) |
| CopyObject REPLACE with no `Content-Type` | `binary/octet-stream` | `binary/octet-stream` | No `Content-Type` |
| `x-amz-copy-source-if-match` on CopyObject, with another ETag | 412 PreconditionFailed | 412 PreconditionFailed | 412 PreconditionFailed |
| `x-amz-copy-source-if-match` on CopyObject of an older version | Compared with the ETag of that version | Compared with the ETag of that version | 404 NoSuchKey (no versions) |
| CopyObject REPLACE of a key that does not exist (the start probe of the metadata edit) | 404 NoSuchKey, no object | 404 NoSuchKey, no object | 404 NoSuchKey, no object |
| Lifecycle: expiration after N days, with a prefix | Yes | Yes | Yes |
| Lifecycle: abort of incomplete multipart uploads | Yes | 400 InvalidArgument. With an expiration in the same rule: dropped | Yes |
| Lifecycle: expiration of noncurrent versions | Yes | Yes | Dropped |
| Lifecycle: tag filter | Yes | Yes | Dropped: the rule applies to all the prefix |
| Lifecycle: removal of expired delete markers | Yes | Yes | 400 InvalidRequest |
| Lifecycle: transition to a storage class | 501 NotImplemented | 400 InvalidStorageClass | Dropped |
| Lifecycle: expiration on a date, size filter (`ObjectSizeGreaterThan`) | Yes | Yes. The children of `And` come back in another order | Yes |
| Lifecycle: the old `Prefix` element outside of `Filter` | Yes | Yes | Dropped: the rule applies to the whole bucket |
| Lifecycle: a rule with no ID | Yes, with no ID | Yes. The provider gives the rule an ID | Yes, with no ID |
| Lifecycle: two rules with one ID | Yes | 400 InvalidArgument | Yes |
| Lifecycle: a configuration with no rule | Yes | 400 InvalidArgument | 400 MalformedXML |
| Lifecycle: an empty `Filter` | Yes | Yes | The provider removes the `Filter` |
| Effect of the lifecycle rules | A lifecycle worker in a daily pass. The TTL at write is off by default, and an object written after a rule has no TTL | Not measured | Not measured |
| Header `Server` | `SeaweedFS 30GB 4.41` | `MinIO` | None. No request id. The error document has `Region` and no `RequestId` |
| Object Lock: bucket with lock, legal hold, retention | Yes. A refusal gives 403 AccessDenied | Yes. A refusal gives 400 InvalidRequest | 501 NotImplemented |
| Creation of a bucket with Object Lock | `s3.bucket.create -withLock` in `weed shell`. It turns on the versioning | `x-amz-bucket-object-lock-enabled: true` at CreateBucket. It turns on the versioning | 403 AccessDenied: the key cannot make buckets |
| GetObjectLockConfiguration on a bucket with no lock | 404 ObjectLockConfigurationNotFoundError | 404 ObjectLockConfigurationNotFoundError | 501 NotImplemented |
| GetObjectLockConfiguration on a bucket with lock | 200, `ObjectLockEnabled` Enabled, and the default retention when it has one | 200, the same | 501 NotImplemented |
| GetObjectRetention of a version with no retention | 404 ObjectLockConfigurationNotFoundError | 400 NoSuchObjectLockConfiguration | 501 NotImplemented |
| GetObjectLegalHold of a version with no legal hold | 404 NoSuchObjectLegalHold. After a legal hold OFF: 200 with OFF | 400 NoSuchObjectLockConfiguration. After a legal hold OFF: 200 with OFF | 501 NotImplemented |
| `RetainUntilDate` of GetObjectRetention | Seconds (`2026-10-20T15:56:32Z`) | Milliseconds (`2026-10-20T15:56:31.000Z`) | - |
| Lock headers of HEAD | `x-amz-object-lock-mode`, `x-amz-object-lock-retain-until-date`, `x-amz-object-lock-legal-hold` (also OFF) | The same. `x-amz-object-lock-legal-hold` only when it is ON | - |
| Delete of a version with a legal hold or an active retention | 403 AccessDenied, also with `x-amz-bypass-governance-retention` for a legal hold | 400 InvalidRequest, "Object is WORM protected and cannot be overwritten" | - |
| DeleteObjects of a locked version | 200 with an `Error` for the key: AccessDenied | 200 with an `Error` for the key: InvalidRequest | - |
| Delete with no `versionId` of a locked object (DELETE and DeleteObjects) | Yes: a delete marker. The locked version stays | Yes: a delete marker. The locked version stays | - |
| PutObjectRetention: a later date in the same mode | Yes | Yes | - |
| PutObjectRetention: an earlier Governance date | 403 AccessDenied. With the bypass header: yes | 400 InvalidRequest. With the bypass header: yes | - |
| PutObjectRetention: Governance to Compliance with a later date | 403 AccessDenied. With the bypass header: yes | 400 InvalidRequest. With the bypass header: yes | - |
| PutObjectRetention: an earlier Compliance date, or Compliance to Governance | 403 AccessDenied, also with the bypass header | 400 InvalidRequest, also with the bypass header | - |
| PutObjectRetention: a date in the past | 400 InvalidRequest | 400 MalformedXML | - |
| PutObjectRetention with an empty `Retention` (removal) | 400 InvalidRequest, also with the bypass header | Yes with the bypass header for Governance. Without it: 400 InvalidRequest | - |
| Delete of a version after the end of its Compliance retention | Yes | Yes | - |
| PutObjectLegalHold, GetObjectRetention on a bucket with no lock | 400 InvalidRequest | 400 InvalidRequest, "Bucket is missing ObjectLockConfiguration" | 501 NotImplemented |
| Usage of a bucket (admin API) | Master `GET /vol/status`: `Size` and `DeletedByteCount` of the volumes of the collection of the bucket. Each volume adds 8 bytes. A delay of some seconds. No count of objects | `GET /minio/admin/v3/datausageinfo` (SigV4, service `s3`): `size` and `objectsCount`. A background scan, about one time each minute | `GET /v2/GetBucketInfo?globalAlias=` (bearer token): `bytes` and `objects`, at once on one node |
| Quota of a bucket (admin API) | Filer `GET /buckets/<bucket>?metadata=true`: `Quota` in bytes, 0 for none. Set with `s3.bucket.quota`, enforced only after `s3.bucket.quota.enforce -apply` | `GET /minio/admin/v3/get-bucket-quota?bucket=`: `quota` (and `size`) in bytes, 0 for none. A hard quota refuses a write | `quotas.maxSize` and `quotas.maxObjects`, `null` for none |
| Admin request with a wrong credential | No credential (closed network) | 403 | 403 |
| SSE-S3 (`x-amz-server-side-encryption: AES256`) on PutObject, CreateMultipartUpload and CopyObject | Yes, with a KEK (`WEED_S3_SSE_KEK`). Without it: 500 InternalError, no object | Yes, with a KMS (`MINIO_KMS_SECRET_KEY`). Without it: 501 NotImplemented | Dropped: the provider answers 200, does not encrypt, and gives no encryption header |
| Encryption header in the answer of PutObject with SSE-S3 | `AES256` | `AES256` | None |
| CopyObject of an SSE-S3 object without the header | The copy is not encrypted | The copy is not encrypted | - |
| SSE-KMS (`aws:kms`) on PutObject | 500 InternalError | Yes, with the KMS key of MinIO (`arn:aws:kms:<name>`) | Not tested |
| SSE-C on PutObject over HTTP | Yes | 400 InvalidRequest: SSE-C needs TLS | Yes |
| HEAD of an SSE-C object without the key | 400 with no body. The answer has `x-amz-server-side-encryption-customer-algorithm` | - | 400 with no body |
| GET of an SSE-C object without the key | 400 InvalidArgument, "...Customer provided keys must provide the customer key." | - | 400 InvalidRequest, "Bad request: Object is encrypted" |
| ListObjectsV2 of an SSE-C object | Yes, with the size | - | Yes, with the size |
| GetBucketEncryption on a bucket with no default | 404 ServerSideEncryptionConfigurationNotFoundError | 404 ServerSideEncryptionConfigurationNotFoundError | 501 NotImplemented |
| PutBucketEncryption with AES256 | Yes. A later PutObject with no header gets AES256 | Yes (tested with Content-MD5). The effect on a PutObject is not tested | Not tested |

Notes for the code:

- Garage answers 501 NotImplemented for each function that it does not have. The message is `Unimplemented action: <name>`.
- Do not send a `versionId` to Garage. Garage gives an `x-amz-version-id` header on each write, but it keeps no versions.
- Garage drops some lifecycle settings with no error. After a write, read the configuration again and compare it.
  The lifecycle editor offers only the fields that each provider keeps. Refer to the section "Edit of the lifecycle
  rules" of [docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md).
- Delete the lifecycle configuration with `DeleteBucketLifecycle`: MinIO and Garage refuse a configuration with no rule.
- Send `Content-MD5` or an `x-amz-checksum-*` header with DeleteObjects: MinIO refuses the request without one.
- On MinIO, list the multipart uploads with no prefix. Then Stowr filters the keys.
- The lock refusal codes are different: 403 AccessDenied on SeaweedFS, 400 InvalidRequest on MinIO. The bypass header does not remove a legal hold or a Compliance retention. After such a refusal, read the lock of the version: the code alone does not tell a lock from a missing permission.
- A delete with no `versionId` in a bucket with Object Lock writes a delete marker. Only the delete of a version can meet a lock.
- SSE-S3 needs a key service: a KEK for SeaweedFS (`WEED_S3_SSE_KEK`, or `s3.sse.kek` in `security.toml`), a KMS for MinIO. Without it, the write fails. Garage accepts the header and does not encrypt: Stowr writes a warning at start for a Garage bucket with `encryption = "sse-s3"`.
- A copy without `x-amz-server-side-encryption` loses the SSE-S3 of its source on SeaweedFS and MinIO. Send the header with each copy.
