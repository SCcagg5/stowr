# The test stack has no edge and no PDP: the server checks no identity token.
access {
  mode = "open"
}

server {
  listen                               = ":8080"
  job_history_limit                    = 10
  memory_limit_bytes                   = 67108864
  max_range_cache_bytes                = 8388608
  max_concurrent_storage_requests      = 4
  max_concurrent_requests_per_storage  = 2
}

auth "seaweedfs" {
  provider               = "s3"
  mode                   = "access_key"
  endpoint               = "http://seaweedfs:8333"
  region                 = "us-east-1"
  access_key_id          = "GK0123456789abcdef01234567"
  # A secret accepts only _env and _file. The test compose and scratch-runtime.sh mount test/secrets here.
  secret_access_key_file = "/config/secrets/s3_secret_access_key"
}

bucket "seaweedfs-main" {
  name           = "SeaweedFS - primary"
  auth           = "seaweedfs"
  bucket         = "default"
  permissions    = ["read", "write", "delete"]
  max_scan_pages = 15
}

bucket "seaweedfs-archive" {
  name           = "SeaweedFS - archive"
  auth           = "seaweedfs"
  bucket         = "archive"
  permissions    = ["read", "write", "delete"]
  max_scan_pages = 15
}
