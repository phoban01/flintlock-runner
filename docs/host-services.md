# Host Services and what Jobs share

Every Host runs four Host Services on its guest bridge: `buildkitd`, a Go
module proxy, a pull-through registry mirror and an HTTP cache
(`docs/requirements/06-fleet.md`, section "Host services"). The Executor
points each Job at the services of the Host its MicroVM runs on, and only
that Host (SE-053). This page says what that sharing means for the Jobs on
one Host, as `docs/requirements/09-security.md` requires the Runner to
document (SE-054, SE-056).

The trust boundary is the Host. Every guest on a Host is a Job of this
Runner, and traffic to the bridge gateway never leaves the Host, so the
guest firewall rather than a per-Job credential is what keeps other Hosts
and the outside world off these services (SE-051). Within one Host the
caches are shared on purpose: that is what lets a Job in a fresh MicroVM
start warm.

## buildkitd layer cache

The buildkitd layer cache is shared between Jobs on the same Host: a Job can read the layers another Job on that Host produced.

A Job that builds an image through `BUILDKIT_HOST` reuses any layer an
earlier Job on the same Host built, and a later Job can reuse the layers it
builds. A build secret that ends up in a layer, rather than being passed
with `--secret`, is therefore readable by the other Jobs on that Host.
`buildkitd` runs rootless as an unprivileged user (SE-050), so a build that
escapes its sandbox does not gain root on the Host, but the cache itself is
common to every Job placed there.

## Private Go modules

Private modules cached by a Host's Go module proxy are readable by every Job that runs on that Host.

The Go module proxy fetches private modules with one read-only credential
per fleet, scoped to the groups named in the private module patterns
(SE-055), never with a Job's token (SE-057). Once a private module is in the
proxy's cache, any Job on that Host can fetch it through `GOPROXY`, whether
or not that Job's own token could read the repository. The exposure is
bounded to source that the fleet's credential can reach.

## Opting out

This is the same trust boundary as a shared runner host with a Docker
socket, narrowed to one bare-metal Host. A team that needs per-Job
isolation can:

- run `buildkitd` inside its own Job image, or set `BUILDKIT_HOST` to an
  empty value in the Job, and lose the warm layer cache for that Job;
- set `GOPROXY` (and `GOPRIVATE` or `GONOPROXY`) in the Job itself so that
  private modules are fetched with the Job's own credentials, and lose the
  warm module cache for them.

A variable the Job sets itself always wins over the Host Service variable of
the same name.
