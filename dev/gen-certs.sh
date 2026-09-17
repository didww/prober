#!/bin/sh
# Generate a local dev CA and a backend server certificate for localhost, so
# `make dev` exercises real TLS (there is deliberately no insecure mode). Only
# for development; never ship these.
set -e
cd "$(dirname "$0")"
if [ -f backend.crt ] && [ -f ca.crt ]; then exit 0; fi

# A tiny CA.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes -keyout ca.key -out ca.crt -days 3650 \
  -subj "/CN=prober-dev-ca" >/dev/null 2>&1

# A server cert for localhost, signed by it.
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout backend.key -out backend.csr -subj "/CN=localhost" >/dev/null 2>&1
openssl x509 -req -in backend.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out backend.crt -days 3650 \
  -extfile /dev/stdin >/dev/null 2>&1 <<EXT
subjectAltName = DNS:localhost, IP:127.0.0.1, IP:::1
EXT
rm -f backend.csr ca.srl
echo "dev certs generated in dev/"
