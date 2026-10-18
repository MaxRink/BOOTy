---
section: Behaviour changes in all modes
---

- The default, gobgp and slim ramdisks no longer ship `curl` and its
  libraries (libcurl, Kerberos, LDAP, SASL, libssh2, nghttp2/3, brotli and,
  in slim, OpenSSL and GnuTLS). BOOTy never ran `curl`: it fetches images,
  CAPRF callbacks and tokens with Go's `net/http`. In the rescue shell use
  busybox `wget` (HTTP and HTTPS, `--header` for a bearer token). busybox
  `wget` does not verify TLS certificates: busybox 1.38 prints "TLS
  certificate validation not implemented" and connects to any server
  (#814).
