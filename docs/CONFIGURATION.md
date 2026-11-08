# Configuration

The application accepts one HCL file through `-c <path>` or `-config <path>`. Runtime settings and provider credentials come from this file only. An attribute with an `_env` form reads the environment variable that the file names, and no other variable.

[configuration-reference.md](configuration-reference.md) is the generated list of every key, with its default, its range and its forms. `stowr print-config -c <path>` (or `stowr -c <path> --print-config`) checks the file, writes the value and the source of each key, and exits. The server writes the same values in its log at start. Secrets are hidden.

Supported top-level blocks:

- exactly one `access` block (refer to "Access");
- zero or one `server` block;
- zero or more `auth "id"` blocks;
- zero or more `bucket "id"` blocks. Each `bucket` block needs an `auth` block;
- zero or more `volume "id"` blocks: a folder of the server that the browser shows as a bucket.

A file needs one `bucket` block or one `volume` block at least.

Unknown blocks, attributes, nested blocks, and wrong value types are rejected. The HCL file is limited to 4 MiB. Individual secret files are limited to 1 MiB; a GCS service-account file is bounded separately by the provider parser.

## Server block

```hcl
server {
  listen            = ":8080"
  job_history_limit = 10
  access_mode       = "inherit_credentials"
  log_mode          = "anonymous"

  memory_limit_bytes = 67108864

  max_storage_bytes_per_request       = 274877906944
  max_storage_requests_per_request    = 4096
  max_range_cache_bytes               = 8388608
  max_concurrent_storage_requests     = 4
  max_concurrent_requests_per_storage = 2

  session_ttl_seconds = 1200
  max_stats_folders   = 1000
  max_archive_entries = 10000

  max_background_storage_bytes    = 274877906944
  max_background_storage_requests = 100000
}
```

| Attribute | Default | Description |
|---|---:|---|
| `listen` | `:8080` | Numeric `host:port` listen address. URL syntax is rejected. |
| `job_history_limit` | `10` | Number of terminal job results retained in process memory. Valid range: 1-20. |
| `access_mode` | `inherit_credentials` | `inherit_credentials` or `force_read_only`. |
| `log_mode` | `anonymous` | `anonymous` avoids object keys in request logs; `detailed` is for controlled diagnostics. |
| `allow_full_object_fallback` | `false` | Allows explicitly implemented parser fallbacks that require a complete object. It never changes strict Range validation into an implicit oversized response. |
| `memory_limit_bytes` | `0` | Optional Go soft memory limit. `0` leaves the Go runtime default unchanged. |
| `max_storage_bytes_per_request` | 256 GiB | Provider bytes allowed for one interactive HTTP request. |
| `max_storage_requests_per_request` | `4096` | Provider calls allowed for one interactive HTTP request. |
| `max_range_cache_bytes` | 8 MiB | Maximum in-memory cache used by a range reader session. |
| `max_concurrent_storage_requests` | `4` | Global provider-request concurrency. |
| `max_concurrent_requests_per_storage` | `2` | Provider-request concurrency for one bucket. |
| `session_ttl_seconds` | `1200` | In-memory preview/session TTL. |
| `max_stats_folders` | `1000` | Maximum exact folder aggregates retained by one Insights result. |
| `max_archive_entries` | `10000` | Maximum deterministic central-directory entries and selected archive members. |
| `max_background_storage_bytes` | 256 GiB | Provider-byte budget for an explicit full-object job. |
| `max_background_storage_requests` | `100000` | Provider-call budget for an explicit background job. |

The server never creates a data directory. All sessions and jobs are memory-only. At most four jobs may be active or queued at once.

### Browser interface

The binary contains one browser interface. No key selects it.

## S3 authentication

```hcl
auth "primary-s3" {
  provider               = "s3"
  mode                   = "access_key"
  endpoint               = "https://s3.example.internal"
  region                 = "eu-west-1"
  access_key_id_file     = "/run/secrets/s3-access-key-id"
  secret_access_key_file = "/run/secrets/s3-secret-access-key"
  session_token_file     = "/run/secrets/s3-session-token"
  insecure_skip_verify   = false
}
```

S3 modes:

- `access_key`: requires `access_key_id` or `access_key_id_file` and `secret_access_key` or `secret_access_key_file`; `session_token` is optional.
- `anonymous`: rejects access-key fields.

A secret (`secret_access_key`, `session_token`) accepts only its `_env` form (the name of an environment variable) or its `_file` form. A secret in clear text in the file stops the start. `access_key_id` is an identifier, not a secret: it also accepts the value in the file. Give one form only. File paths relative to the HCL file are resolved from the HCL directory. The value of a file applies without its last line end; a secret with a space or a control character stops the start.

The server never changes a value to make it fit. It does not remove spaces, does not change the case (`provider = "S3"` is an error), does not remove the `/` at the end of `endpoint`, and does not add or remove a `/` in `root_prefix` (`root_prefix` is `""` or a prefix such as `"published/"`). The error gives the key, the expected form and the value.

The endpoint must use HTTP or HTTPS and may not contain credentials, a query string, or a fragment. Redirects and process environment proxies are disabled.

## Usage of the buckets

A `usage` block in an `s3` auth block gives the usage and the quota of its buckets, and the capacity of the storage behind them (the disks or the nodes of the provider). The bucket selector shows them in the facts of each bucket, and warns about a bucket that uses 90 % or more of its limit. Without the block, the server makes no admin request and the interface shows no usage. Refer to "Usage and quota of a bucket" in [ARCHITECTURE.md](ARCHITECTURE.md).

```hcl
auth "garage" {
  provider               = "s3"
  endpoint               = "https://garage.example.internal"
  region                 = "garage"
  access_key_id_file     = "/run/secrets/garage-key-id"
  secret_access_key_file = "/run/secrets/garage-secret"

  usage {
    source     = "garage"
    endpoint   = "http://garage.example.internal:3903"
    token_file = "/run/secrets/garage-admin-token"
  }
}
```

| Source | `endpoint` | `filer_endpoint` | `token` | Credentials |
|---|---|---|---|---|
| `garage` | Required: the admin API (port 3903 by default) | Not allowed | Required, `token_env` or `token_file` only: the admin token | The bearer token |
| `seaweedfs` | Required: the master (port 9333 by default) | Required: the filer (port 8888 by default) | Not allowed | None: keep the master and the filer on a closed network |
| `minio` | Optional: the endpoint of the auth block by default | Not allowed | Not allowed | The S3 credentials of the auth block, mode `access_key` only. They need `admin:GetBucketQuota`, `admin:DataUsageInfo` and `admin:StorageInfo` |

For the capacity of the storage, the reader of `seaweedfs` also asks each volume server that the master gives (`/status`): keep them on the closed network of the master. Write `source` in lower case: the server does not change its case. A key that another source needs is a contradiction and stops the start. A `usage` block in a `gcs` auth block also stops the start. AWS S3 has no quota: give no block. The URLs use HTTP or HTTPS, without credentials, query, fragment or a `/` at the end. `print-config` shows the URLs and hides the token.

## GCS authentication

```hcl
auth "gcs-service-account" {
  provider         = "gcs"
  mode             = "service_account"
  credentials_file = "/run/secrets/gcs-service-account.json"
}
```

GCS modes:

- `service_account`: requires `credentials_file`;
- `anonymous`: rejects `credentials_file`.

If omitted, the GCS endpoint defaults to `https://storage.googleapis.com`.

## Bucket block

```hcl
bucket "archive" {
  name           = "Archive"
  auth           = "primary-s3"
  bucket         = "archive"
  root_prefix    = "published/"
  permissions    = ["read"]
  max_scan_pages = 15
}
```

| Attribute | Default | Description |
|---|---:|---|
| `name` | block id | Frontend display name. |
| `auth` | required | Referenced authentication id. |
| `bucket` | required | Provider bucket name. |
| `root_prefix` | empty | Virtual root exposed by the application. |
| `permissions` | omitted | Optional application ceiling containing `read`, `write`, and/or `delete`. |
| `max_scan_pages` | `1` | Maximum 1,000-entry provider listing pages combined in navigation. `0` means no page-count limit. |
| `quota_bytes` | omitted | A size limit (1 to 2^60 bytes) that the interface shows when the provider gives no limit. It needs a `usage` block in the auth block. The server does not enforce it. |
| `encryption` | `""` | `""` sends no encryption header. `"sse-s3"` sends `x-amz-server-side-encryption: AES256` with each write of an object (upload, copy, move, rename, metadata edit, restore). SSE-C and SSE-KMS are not supported. Not for a `gcs` bucket. Refer to the section "Encryption" of [ARCHITECTURE.md](ARCHITECTURE.md). |

`read` permits list, preview, details, inspection, version listing, and download. `write` permits upload, destination-side object creation, and the edit of the tags and the metadata of an object. `delete` permits deletion and the source side of move/rename. Provider policy and retention rules still apply.

## Volume block

A `volume` block shows a folder of the server as a bucket: a Docker volume, an NFS or CephFS mount, or a FUSE mount
such as `weed mount` of SeaweedFS. It needs no `auth` block. The section "Volumes" of
[ARCHITECTURE.md](ARCHITECTURE.md) gives the rules of the paths, the listings, the writes and the uploads.

```hcl
volume "shared" {
  name           = "Shared files"
  path           = "/data/shared"
  root_prefix    = "published/"
  permissions    = ["read", "write", "delete"]
  max_scan_pages = 15
}
```

| Attribute | Default | Description |
|---|---:|---|
| `name` | block id | The name that the interface shows. |
| `path` | required | The absolute path of the folder, without `/` at the end and without an empty, `.` or `..` part. The server does not clean the value. The folder must exist at start, and the process must read it. |
| `permissions` | omitted | The ceiling of the operations: `read`, `write` and `delete`. Without the key, the three apply. |
| `root_prefix` | empty | The folder of `path` that the browser shows: a relative path that ends with `/`. It must exist at start. A symbolic link that leaves it is not followed. |
| `max_scan_pages` | `1` | The 1000-entry pages of a folder that one listing combines. `0` means no limit. |

These rules stop the start:

- A folder that does not exist, a file in place of the folder, or a folder that the process cannot read.
- A folder that the process cannot write when the permissions include `write` or `delete`, or when the block has no
  `permissions`. The start makes and removes a temporary file to check it. Give `permissions = ["read"]` for a folder
  that is read only. With `server.access_mode = "force_read_only"`, the start makes no such check.
- A `volume` block and a `bucket` block with the same label.
- An `auth` block with the label `volume` in a file with a `volume` block: `volume` is the address word of the
  volumes in the interface (`/-/volume/<id>/`).

The address of a volume in the interface is `/-/volume/<id>/`. The process of Stowr reads and writes the files: give it the owner or the group of the folder. In a container, mount the folder with
write access, and keep the root file system read only.

## Access

The `access` block is mandatory. A file without it stops the start. The block sets who can use Stowr. Its key `mode` has two values.

- `authzen`: Stowr checks the identity token that the edge signs. It asks a PDP (Policy Decision Point) for each operation.
  The blocks `identity` and `pdp` are mandatory.
- `open`: Stowr checks no token and asks no PDP. The two blocks are not allowed. Use this mode only behind a reverse proxy that
  authenticates the users. Stowr writes a warning at start.

```hcl
access {
  mode = "authzen"

  identity {
    issuer           = "https://edge.example"
    audience         = "infra-s3"
    public_keys_file = "/run/keys/edge-jwks.json"
  }

  pdp {
    url        = "https://pdp.example:4182"
    token_file = "/run/secrets/pdp-token"
  }
}
```

```hcl
access {
  mode = "open"
}
```

[configuration-reference.md](configuration-reference.md) gives each key of the block with its default, its range and its forms.
[ACCESS.md](ACCESS.md) gives the rules of the identity token, the resource tree, the actions, the routes, the budgets and the
errors.

The key `server.access_mode` is a storage ceiling of the process: `force_read_only` refuses each write and each delete for all
the people. The `access` block sets the access of each person. The two apply together, and `server.access_mode` is checked first.

## Version capability probe

A single bounded version-list request is made for every bucket when the application starts. A volume has no versions and gets no request. Any error marks version browsing unavailable for that bucket. This includes providers such as Garage that return HTTP 501 for the version-list API. The result is process-local and is exposed only as a boolean capability; the HCL file does not need a versioning flag.

## Public path

There is no public-path attribute. The frontend resolves URLs relative to `document.baseURI`. A reverse proxy can mount the application under any path by stripping the public prefix before forwarding requests to the backend.
