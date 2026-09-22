#!/usr/bin/bash
# Build step of the image stage, after rootfs/ has been copied in: install
# the policy module, enable the units and remove what must not ship.
set -euo pipefail

chmod 0755 /usr/libexec/flr/host-config /usr/libexec/flr/kvm-gate /usr/libexec/flr/thin-pool \
  /usr/libexec/flr/cache-volume /usr/libexec/flr/network /usr/libexec/flr/kubelet-config \
  /usr/libexec/flr/check /usr/libexec/flr/check-thin-pool-cases
chmod 0644 /usr/libexec/flr/lib.sh

#= docs/requirements/11-host-image.md#kernel-and-kvm
#/ The Host Image SHALL NOT disable SELinux globally.
# The base image enforces the targeted policy, and the build leaves
# /etc/selinux/config and the kernel arguments alone. What the image adds is
# one module for one domain, selinux/flr.te. -n installs it into the policy
# store without trying to load it into the build machine's kernel.
semodule -n -i /usr/share/selinux/packages/flr.pp

systemctl enable \
  flr-host-config.service flr-kvm.service flr-thin-pool.service flr-cache.service \
  flr-network.service flr-dnsmasq.service flr-kubelet-config.service \
  containerd.service flintlockd.service kubelet.service
# cloud-init's unit names differ between releases; enable what this one has.
for u in cloud-init-local cloud-init-main cloud-init-network cloud-init cloud-config cloud-final; do
  if [ -e "/usr/lib/systemd/system/$u.service" ]; then
    systemctl enable "$u.service"
  fi
done
# The distribution's dnsmasq.service would serve every interface.
systemctl disable dnsmasq.service

#= docs/requirements/11-host-image.md#kubernetes-node
#/ The Host Image SHALL disable automatic bootc updates, so that
#/ an operating system update is applied only to a drained Host.
systemctl mask bootc-fetch-apply-updates.timer bootc-fetch-apply-updates.service

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image SHALL NOT contain any credential, private key or
#/ token.
# Nothing the build adds is one, and what a package or a first boot might
# leave behind is removed: host keys, the machine id, cloud-init's state and
# any kubelet or kubeadm state. check.sh looks for all of it again.
rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
rm -rf /var/lib/cloud /var/lib/kubelet /etc/kubernetes/pki /root/.ssh
rm -f /etc/kubernetes/*.conf
: >/etc/machine-id
rm -rf /var/cache/* /var/log/* /var/lib/dnf /tmp/*
