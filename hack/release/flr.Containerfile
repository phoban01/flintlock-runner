# The container image of flr (docs/requirements/12-cluster-fleet.md,
# KF-140 and KF-141), built by goreleaser's dockers_v2 from the same
# binaries the flr archives ship. goreleaser puts each platform's binary at
# $TARGETPLATFORM/flr in the build context.
#
# The image is scratch plus exactly three things: the static flr binary,
# the CA certificates and the time zone database. Both of the latter are
# copied from Google's distroless static image, pinned by the digest of its
# multi-platform index; the files are the same on every platform, and the
# stage runs nothing, so no platform is emulated. distroless itself is not
# the base because it also carries /etc/passwd, /etc/group, os-release,
# licences, a dpkg status database, /home and /tmp, which KF-141 excludes.
# To move the pin, resolve the new digest from the registry:
#   crane digest gcr.io/distroless/static-debian12:nonroot
FROM --platform=$BUILDPLATFORM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS certs

#= docs/requirements/12-cluster-fleet.md#cluster-release
#/ The container image of `flr` SHALL run as a non-root user and
#/ SHALL contain nothing but the `flr` binary, certificate authority
#/ certificates and time zone data.
FROM scratch
ARG TARGETPLATFORM
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=certs /usr/share/zoneinfo /usr/share/zoneinfo
COPY --chmod=0555 $TARGETPLATFORM/flr /usr/bin/flr
# A numeric user, so that the kubelet can prove runAsNonRoot without an
# /etc/passwd. 65532 is distroless's nonroot.
USER 65532:65532
ENTRYPOINT ["/usr/bin/flr"]
