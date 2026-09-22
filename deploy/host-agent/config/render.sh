#!/bin/sh
# render.sh: the Host Agent's first init container. It turns the templates of
# the ConfigMap flintlock-host-agent-config into the configuration files of
# the Pod Provider and of every enabled Host Service, for this Host.
#
# The Host's own settings come from /run/flr/host.env, which the Host Image's
# flr-host-config unit writes at every boot from the Host configuration file
# (HI-050) and the image defaults (HI-051). The one that matters most is
# FLR_GATEWAY, the guest bridge gateway address, the first address of the
# guest subnet: every Host Service binds to it and to nothing else (KF-071).
# The file is parsed, never sourced.
#
# The templates are plain files with @NAME@ placeholders and whole lines of
# the form @BLOCK:name@ that this script expands. Whatever is not a template
# (*.tmpl) is ignored here. Anything invalid stops the pod before any service
# starts, rather than letting one bind the wrong address.
#
# Environment, for the tests in deploy/tests/checks.yaml:
#   FLR_TEMPLATES  the ConfigMap's mount (default /etc/flr/templates)
#   FLR_HOST_ENV   the Host Image's host.env (default /run/flr/host.env)
#   FLR_OUT        where the rendered files go (default /etc/flr/rendered)
set -eu

templates=${FLR_TEMPLATES:-/etc/flr/templates}
host_env=${FLR_HOST_ENV:-/run/flr/host.env}
out=${FLR_OUT:-/etc/flr/rendered}

die() {
  echo "render: $*" >&2
  exit 1
}

# get FILE KEY prints the value of the last KEY=value line of FILE.
get() {
  sed -n "s/^$2=//p" "$1" | tail -n 1
}

is_uint() {
  case $1 in '' | *[!0-9]*) return 1 ;; esac
}

is_ipv4() {
  echo "$1" | grep -Eq '^(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}$'
}

[ -r "$host_env" ] || die "$host_env is missing: flr-host-config has not run on this Host"
gateway=$(get "$host_env" FLR_GATEWAY)
reserve_cpu=$(get "$host_env" FLR_HOST_RESERVE_VCPU)
reserve_mb=$(get "$host_env" FLR_HOST_RESERVE_MEMORY_MB)
is_ipv4 "$gateway" || die "FLR_GATEWAY '$gateway' in $host_env is not an IPv4 address"
is_uint "$reserve_cpu" || die "FLR_HOST_RESERVE_VCPU '$reserve_cpu' in $host_env is not a whole number"
is_uint "$reserve_mb" || die "FLR_HOST_RESERVE_MEMORY_MB '$reserve_mb' in $host_env is not a whole number"

mkdir -p "$out"

# ---- Go module proxy parameters (FL-105, FL-114, FL-115) ----
go_env=$templates/go-proxy.env
go_upstream=
go_private=
go_vcs=
go_revalidate=
if [ -r "$go_env" ]; then
  go_upstream=$(get "$go_env" UPSTREAM)
  go_private=$(get "$go_env" PRIVATE_PATTERNS | tr -d ' ')
  go_vcs=$(get "$go_env" PRIVATE_VCS_HOST)
  go_revalidate=$(get "$go_env" PRIVATE_REVALIDATE_SECONDS)
  echo "$go_upstream" | grep -Eq '^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$' ||
    die "go-proxy.env: UPSTREAM '$go_upstream' is not an https URL"
  if [ -n "$go_private" ]; then
    echo "$go_private" | grep -Eq '^[A-Za-z0-9.*?_~/-]+(,[A-Za-z0-9.*?_~/-]+)*$' ||
      die "go-proxy.env: PRIVATE_PATTERNS '$go_private' is not a comma-separated list of module path patterns"
    echo "$go_vcs" | grep -Eq '^[A-Za-z0-9.-]+(:[0-9]+)?$' ||
      die "go-proxy.env: PRIVATE_PATTERNS need PRIVATE_VCS_HOST, a host name"
    is_uint "$go_revalidate" && [ "$go_revalidate" -gt 0 ] ||
      die "go-proxy.env: PRIVATE_REVALIDATE_SECONDS '$go_revalidate' is not a positive number of seconds"
  fi
fi

# private_regex is internal/fleet/scripts privateRegex: GOPRIVATE-style
# patterns as one regular expression over escaped module paths, matching the
# list and latest queries that FL-115 answers from cache.
private_regex() {
  alts=
  for p in $(echo "$go_private" | tr ',' ' '); do
    p=${p%/}
    # Module paths are ASCII; the case encoding is of ASCII letters only.
    # shellcheck disable=SC2018,SC2019
    a=$(printf '%s' "$p" | sed -e 's/[A-Z]/!&/g' | tr 'A-Z' 'a-z' |
      sed -e 's/[.+(){}|^$\\]/\\&/g' -e 's/\[/\\[/g' -e 's/\]/\\]/g' \
        -e 's/\*/[^\/]*/g' -e 's/?/[^\/]/g')
    alts=${alts:+$alts|}$a
  done
  printf '^/(%s)(/.*)?/@(v/list|latest)$' "$alts"
}

# ---- registry mirror upstreams (FL-104) ----
mirror_upstreams=$templates/registry-mirror.upstreams
upstream_urls() {
  sed -e 's/#.*//' -e 's/[[:space:]]//g' "$mirror_upstreams" | grep -v '^$' || true
}
if [ -r "$mirror_upstreams" ]; then
  [ -n "$(upstream_urls)" ] || die "registry-mirror.upstreams lists no upstream"
  for u in $(upstream_urls); do
    echo "$u" | grep -Eq '^https://[A-Za-z0-9.-]+(:[0-9]+)?/?$' || die "registry-mirror.upstreams: '$u' is not an https registry URL"
  done
fi

# ---- HTTP cache upstreams (FL-106) ----
http_upstreams=$templates/http-cache.upstreams
http_lines() {
  sed -e 's/#.*//' "$http_upstreams" | grep -v '^[[:space:]]*$' || true
}
if [ -r "$http_upstreams" ]; then
  http_lines | while read -r name url size ttl extra; do
    echo "$name" | grep -Eq '^[a-z0-9_]+$' || die "http-cache.upstreams: name '$name' is not [a-z0-9_]+"
    echo "$url" | grep -Eq '^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$' || die "http-cache.upstreams: $name: '$url' is not an https URL"
    echo "$size" | grep -Eq '^[0-9]+[kmgKMG]?$' || die "http-cache.upstreams: $name: size '$size' is not an nginx size"
    is_uint "$ttl" || die "http-cache.upstreams: $name: ttl '$ttl' is not a number of seconds"
    [ -z "$extra" ] || die "http-cache.upstreams: $name: unexpected '$extra'"
  done
fi

# block NAME prints the lines a @BLOCK:NAME@ line expands to.
block() {
  case $1 in
  kubelet-host-services)
    # One fragment per enabled Host Service component (KF-015, KF-017).
    set -- "$templates"/kubelet.host-service.*.yaml
    if [ -e "$1" ]; then
      echo "host_services:"
      cat "$@"
    else
      echo "host_services: {}"
    fi
    ;;
  buildkit-registry-mirrors)
    # FL-103: images resolve through this Host's registry mirror first.
    [ -r "$mirror_upstreams" ] || return 0
    for u in $(upstream_urls); do
      h=${u#https://}
      h=${h%/}
      [ "$h" = registry-1.docker.io ] && h=docker.io
      printf '\n[registry."%s"]\n  mirrors = ["%s:5000"]\n' "$h" "$gateway"
    done
    printf '\n[registry."%s:5000"]\n  http = true\n' "$gateway"
    ;;
  zot-sync-registries)
    sep=
    for u in $(upstream_urls); do
      printf '%s        {"urls": ["%s"], "onDemand": true, "tlsVerify": true, "content": [{"prefix": "**"}]}' "$sep" "$u"
      sep=',
'
    done
    echo
    ;;
  athens-go-env)
    echo "  \"GOPROXY=$go_upstream\","
    echo '  "GOSUMDB=sum.golang.org",'
    if [ -n "$go_private" ]; then
      echo "  \"GOPRIVATE=$go_private\","
      echo "  \"GONOSUMDB=$go_private\","
      echo '  "NETRC=/etc/flr/secrets/go-proxy/netrc",'
    fi
    ;;
  athens-no-sum-patterns)
    [ -n "$go_private" ] || return 0
    printf 'NoSumPatterns = ["%s"]\n' "$(echo "$go_private" | sed 's/,/", "/g')"
    ;;
  goproxy-private-cache-path)
    [ -n "$go_private" ] || return 0
    echo "    # FL-115: private module listings are answered from cache until the"
    echo "    # revalidation interval elapses."
    echo "    proxy_cache_path /var/lib/flintlock-runner/cache/goproxy-lists levels=1:2 keys_zone=flr_goproxy_private:10m inactive=${go_revalidate}s use_temp_path=off;"
    ;;
  goproxy-private-location)
    [ -n "$go_private" ] || return 0
    echo "        location ~ \"$(private_regex)\" {"
    echo "            proxy_pass http://127.0.0.1:3999;"
    echo "            proxy_cache flr_goproxy_private;"
    echo "            proxy_cache_key \$uri;"
    echo "            proxy_cache_valid 200 404 410 ${go_revalidate}s;"
    echo "            proxy_cache_use_stale error timeout updating;"
    echo "        }"
    ;;
  http-cache-paths)
    http_lines | while read -r name url size ttl; do
      echo "    proxy_cache_path /var/lib/flintlock-runner/cache/http/$name levels=1:2 keys_zone=flr_http_$name:10m max_size=$size inactive=${ttl}s use_temp_path=off;"
    done
    ;;
  http-cache-locations)
    http_lines | while read -r name url size ttl; do
      host=${url#https://}
      host=${host%%/*}
      echo "        location /$name/ {"
      echo "            proxy_pass ${url%/}/;"
      echo "            proxy_set_header Host $host;"
      echo "            proxy_ssl_server_name on;"
      echo "            proxy_ssl_verify on;"
      echo "            proxy_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;"
      echo "            proxy_cache flr_http_$name;"
      echo "            proxy_cache_valid 200 301 302 ${ttl}s;"
      echo "            proxy_cache_use_stale error timeout updating;"
      echo "        }"
    done
    ;;
  *)
    die "unknown block $1"
    ;;
  esac
}

# render TEMPLATE OUTPUT
render() {
  tmp=$2.tmp
  : >"$tmp"
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in
    *@BLOCK:*@*)
      name=${line#*@BLOCK:}
      name=${name%%@*}
      block "$name" >>"$tmp"
      ;;
    *)
      printf '%s\n' "$line" | sed \
        -e "s|@BRIDGE_GATEWAY@|$gateway|g" \
        -e "s|@HOST_RESERVE_CPU@|$reserve_cpu|g" \
        -e "s|@HOST_RESERVE_MEMORY@|${reserve_mb}Mi|g" >>"$tmp"
      ;;
    esac
  done <"$1"
  if grep -q '@[A-Z_:]*@' "$tmp"; then
    die "$(basename "$1"): unexpanded placeholder: $(grep -o '@[A-Z_:]*@' "$tmp" | head -n 1)"
  fi
  chmod 0444 "$tmp"
  mv -f "$tmp" "$2"
}

for t in "$templates"/*.tmpl; do
  [ -e "$t" ] || continue
  base=$(basename "$t" .tmpl)
  render "$t" "$out/$base"
  echo "render: $base"
done

# The gateway, for the pre-warm containers.
printf '%s\n' "$gateway" >"$out/gateway"
chmod 0444 "$out/gateway"
echo "render: bridge gateway $gateway, Host reserve ${reserve_cpu} CPU ${reserve_mb}Mi"
