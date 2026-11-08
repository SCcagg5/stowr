# The policy of access. This block is required.
# mode "authzen": the server checks the identity token that the edge sends, and it asks the PDP (policy decision
# point) about each operation. Put the server behind the edge, and limit the network so that only the edge can
# reach it.
access {
  mode = "authzen"

  identity {
    # The header that holds the identity token. The default is X-Identity-Token.
    # header = "X-Identity-Token"
    issuer           = "https://edge.example"
    # The id of this service: the aud of the tokens and the root of each chain of resources.
    audience         = "infra-s3"
    # A JWK Set (RFC 7517) with 1 to 16 keys, Ed25519 or P-256. Give public_keys, public_keys_env or public_keys_file.
    public_keys_file = "/run/keys/edge-jwks.json"
    # The longest life of a token, and the accepted difference of clock. A duration is a whole number and
    # one unit: ms, s, m or h.
    max_age          = "5m"
    clock_skew       = "60s"
  }

  pdp {
    url           = "https://pdp.example:4182"
    # true accepts an http URL. Use it only on a trusted network.
    plain_http    = false
    # The token is a secret: give token_env or token_file.
    token_file    = "/run/secrets/pdp-token"
    timeout       = "500ms"
    # The deadline of a question with context.usage.
    usage_timeout = "2s"
    # 0s keeps no answer of the PDP.
    cache_ttl     = "30s"
    max_evaluations = 64
  }
}

# For a test or a local use without the edge, use mode "open" and no identity block or pdp block. The server then
# checks no identity token and asks no PDP, and it writes a warning at start.
# access {
#   mode = "open"
# }

server {
  listen            = ":8080"
  job_history_limit = 10
  access_mode       = "inherit_credentials"
  log_mode          = "anonymous"

  # This is a soft Go heap target, not a container memory reservation.
  memory_limit_bytes = 67108864

  # Complete user-triggered reads of objects close to 100 GB remain possible,
  # while previews and metadata readers still use exact bounded byte ranges.
  max_storage_bytes_per_request       = 274877906944
  max_storage_requests_per_request    = 4096
  max_range_cache_bytes               = 8388608
  max_concurrent_storage_requests     = 4
  max_concurrent_requests_per_storage = 2

  session_ttl_seconds = 1200
  max_stats_folders   = 1000
  max_archive_entries = 10000

  # These limits apply only to explicit complete-object operations such as
  # per-object integrity verification and selected archive extraction.
  max_background_storage_bytes    = 274877906944
  max_background_storage_requests = 100000
}

# One authentication can be reused by any number of buckets. Each bucket keeps
# its own root, permission ceiling, and navigation scan limit.
auth "primary-s3" {
  provider               = "s3"
  mode                   = "access_key"
  endpoint               = "https://s3.example.internal"
  region                 = "eu-west-1"
  access_key_id_file     = "/run/secrets/s3-access-key-id"
  secret_access_key_file = "/run/secrets/s3-secret-access-key"

  # The usage and the quota of the buckets of this block, from the admin API of the provider
  # (docs/ARCHITECTURE.md, "Usage and quota of a bucket"). Give one block for Garage, SeaweedFS or
  # MinIO. Give no block for AWS S3: it has no quota. The source is never guessed. For Garage:
  # usage {
  #   source     = "garage"
  #   endpoint   = "http://garage.example.internal:3903"
  #   token_file = "/run/secrets/garage-admin-token"
  # }
  # For SeaweedFS: the master gives the size of the volumes, the filer gives the quota.
  # usage {
  #   source         = "seaweedfs"
  #   endpoint       = "http://seaweedfs-master.example.internal:9333"
  #   filer_endpoint = "http://seaweedfs-filer.example.internal:8888"
  # }
  # For MinIO: the admin API on the S3 endpoint, signed with the credentials of this block. They
  # need the admin actions admin:GetBucketQuota and admin:DataUsageInfo.
  # usage {
  #   source = "minio"
  # }
}

bucket "documents" {
  name           = "Documents"
  auth           = "primary-s3"
  bucket         = "documents"
  permissions    = ["read", "write", "delete"]
  max_scan_pages = 15
  # Each write of an object asks the provider for SSE-S3 (x-amz-server-side-encryption: AES256).
  # Remove the key to send no encryption header. SSE-C and SSE-KMS are not supported.
  encryption     = "sse-s3"
}

bucket "archive" {
  name           = "Archive"
  auth           = "primary-s3"
  bucket         = "archive"
  permissions    = ["read"]
  root_prefix    = "published/"
  max_scan_pages = 0
  # A size limit that the interface shows when the provider gives none. It needs a usage block in
  # the auth block. The server does not enforce it.
  # quota_bytes  = 107374182400
}

# A separate authentication may use another provider.
auth "gcs-service-account" {
  provider         = "gcs"
  mode             = "service_account"
  credentials_file = "/run/secrets/gcs-service-account.json"
}

bucket "gcs-backups" {
  name           = "GCS backups"
  auth           = "gcs-service-account"
  bucket         = "company-backups"
  permissions    = ["read", "write"]
  max_scan_pages = 1
}

# A volume: a folder of the server that the browser shows as a bucket, for example a Docker
# volume, an NFS or CephFS mount, or a FUSE mount of SeaweedFS (weed mount). It needs no auth
# block. The folder must exist at start (docs/CONFIGURATION.md, "Volume block"). With write or
# delete in permissions, the start writes and removes a test file: give ["read"] for a folder
# that the process cannot write. The address in the interface is /-/volume/shared/.
# volume "shared" {
#   name           = "Shared files"
#   path           = "/data/shared"
#   permissions    = ["read", "write", "delete"]
#   root_prefix    = "published/"
#   max_scan_pages = 15
# }
