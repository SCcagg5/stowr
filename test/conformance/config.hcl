server {
  listen = "127.0.0.1:8080"
}

access {
  mode = "authzen"

  identity {
    header           = "X-Identity-Token"
    issuer           = "https://edge.example"
    audience         = "infra-s3"
    public_keys_file = "/conformance/edge-jwks.json"
    max_age          = "5m"
    clock_skew       = "30s"
  }

  pdp {
    url           = "http://127.0.0.1:8181"
    plain_http    = true
    token_file    = "/conformance/pdp-token"
    timeout       = "2s"
    usage_timeout = "3s"
    cache_ttl     = "0s"
  }
}

volume "conf" {
  path = "/data"
}
