// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package router

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tailscale/wireguard-go/device"
	"github.com/tailscale/wireguard-go/tun"
	"github.com/tailscale/wireguard-go/wgcfg"
	"tailscale.com/atomicfile"
	"tailscale.com/types/logger"
)

// For now this router only supports the WireGuard userspace implementation.
// There is an experimental kernel version in the works for OpenBSD:
// https://git.zx2c4.com/wireguard-openbsd.

type openbsdRouter struct {
	logf    logger.Logf
	tunname string
	local   wgcfg.CIDR
	routes  map[wgcfg.CIDR]struct{}
}

func newUserspaceRouter(logf logger.Logf, _ *device.Device, tundev tun.Device) (Router, error) {
	tunname, err := tundev.Name()
	if err != nil {
		return nil, err
	}
	return &openbsdRouter{
		logf:    logf,
		tunname: tunname,
	}, nil
}

func cmd(args ...string) *exec.Cmd {
	if len(args) == 0 {
		log.Fatalf("exec.Cmd(%#v) invalid; need argv[0]\n", args)
	}
	return exec.Command(args[0], args[1:]...)
}

func (r *openbsdRouter) Up() error {
	ifup := []string{"ifconfig", r.tunname, "up"}
	if out, err := cmd(ifup...).CombinedOutput(); err != nil {
		r.logf("running ifconfig failed: %v\n%s", err, out)
		return err
	}
	return nil
}

func (r *openbsdRouter) Set(rs Settings) error {
	// TODO: support configuring multiple local addrs on interface.
	if len(rs.LocalAddrs) != 1 {
		return errors.New("freebsd doesn't support setting multiple local addrs yet")
	}
	localAddr := rs.LocalAddrs[0]

	var errq error

	if localAddr != r.local {
		if r.local != (wgcfg.CIDR{}) {
			addrdel := []string{"ifconfig", r.tunname,
				"inet", r.local.String(), "-alias"}
			out, err := cmd(addrdel...).CombinedOutput()
			if err != nil {
				r.logf("addr del failed: %v: %v\n%s", addrdel, err, out)
				if errq == nil {
					errq = err
				}
			}

			routedel := []string{"route", "-q", "-n",
				"del", "-inet", r.local.String(),
				"-iface", r.local.IP.String()}
			if out, err := cmd(routedel...).CombinedOutput(); err != nil {
				r.logf("route del failed: %v: %v\n%s", routedel, err, out)
				if errq == nil {
					errq = err
				}
			}
		}

		addradd := []string{"ifconfig", r.tunname,
			"inet", localAddr.String(), "alias"}
		out, err := cmd(addradd...).CombinedOutput()
		if err != nil {
			r.logf("addr add failed: %v: %v\n%s", addradd, err, out)
			if errq == nil {
				errq = err
			}
		}

		routeadd := []string{"route", "-q", "-n",
			"add", "-inet", localAddr.String(),
			"-iface", localAddr.IP.String()}
		if out, err := cmd(routeadd...).CombinedOutput(); err != nil {
			r.logf("route add failed: %v: %v\n%s", routeadd, err, out)
			if errq == nil {
				errq = err
			}
		}
	}

	newRoutes := make(map[wgcfg.CIDR]struct{})
	for _, route := range rs.Routes {
		newRoutes[route] = struct{}{}
	}
	for route := range r.routes {
		if _, keep := newRoutes[route]; !keep {
			net := route.IPNet()
			nip := net.IP.Mask(net.Mask)
			nstr := fmt.Sprintf("%v/%d", nip, route.Mask)
			routedel := []string{"route", "-q", "-n",
				"del", "-inet", nstr,
				"-iface", localAddr.IP.String()}
			out, err := cmd(routedel...).CombinedOutput()
			if err != nil {
				r.logf("route del failed: %v: %v\n%s", routedel, err, out)
				if errq == nil {
					errq = err
				}
			}
		}
	}
	for route := range newRoutes {
		if _, exists := r.routes[route]; !exists {
			net := route.IPNet()
			nip := net.IP.Mask(net.Mask)
			nstr := fmt.Sprintf("%v/%d", nip, route.Mask)
			routeadd := []string{"route", "-q", "-n",
				"add", "-inet", nstr,
				"-iface", localAddr.IP.String()}
			out, err := cmd(routeadd...).CombinedOutput()
			if err != nil {
				r.logf("addr add failed: %v: %v\n%s", routeadd, err, out)
				if errq == nil {
					errq = err
				}
			}
		}
	}

	r.local = localAddr
	r.routes = newRoutes

	if err := r.replaceResolvConf(rs.DNS, rs.DNSDomains); err != nil {
		errq = fmt.Errorf("replacing resolv.conf failed: %v", err)
	}

	return errq
}

func (r *openbsdRouter) Close() error {
	out, err := cmd("ifconfig", r.tunname, "down").CombinedOutput()
	if err != nil {
		r.logf("running ifconfig failed: %v\n%s", err, out)
	}

	if err := r.restoreResolvConf(); err != nil {
		r.logf("failed to restore system resolv.conf: %v", err)
	}

	return nil
}

const (
	tsConf     = "/etc/resolv.tailscale.conf"
	backupConf = "/etc/resolv.pre-tailscale-backup.conf"
	resolvConf = "/etc/resolv.conf"
)

func (r *openbsdRouter) replaceResolvConf(servers []wgcfg.IP, domains []string) error {
	if len(servers) == 0 {
		return r.restoreResolvConf()
	}

	// Write the tsConf file.
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "# resolv.conf(5) file generated by tailscale\n")
	fmt.Fprintf(buf, "# DO NOT EDIT THIS FILE BY HAND -- CHANGES WILL BE OVERWRITTEN\n\n")
	for _, ns := range servers {
		fmt.Fprintf(buf, "nameserver %s\n", ns)
	}
	if len(domains) > 0 {
		fmt.Fprintf(buf, "search "+strings.Join(domains, " ")+"\n")
	}
	tf, err := ioutil.TempFile(filepath.Dir(tsConf), filepath.Base(tsConf)+".*")
	if err != nil {
		return err
	}
	tempName := tf.Name()
	tf.Close()

	if err := atomicfile.WriteFile(tempName, buf.Bytes(), 0644); err != nil {
		return err
	}
	if err := os.Rename(tempName, tsConf); err != nil {
		return err
	}

	if linkPath, err := os.Readlink(resolvConf); err != nil {
		// Remove any old backup that may exist.
		os.Remove(backupConf)

		// Backup the existing /etc/resolv.conf file.
		contents, err := ioutil.ReadFile(resolvConf)
		if os.IsNotExist(err) {
			// No existing /etc/resolv.conf file to backup.
			// Nothing to do.
			return nil
		} else if err != nil {
			return err
		}
		if err := atomicfile.WriteFile(backupConf, contents, 0644); err != nil {
			return err
		}
	} else if linkPath != tsConf {
		// Backup the existing symlink.
		os.Remove(backupConf)
		if err := os.Symlink(linkPath, backupConf); err != nil {
			return err
		}
	} else {
		// Nothing to do, resolvConf already points to tsConf.
		return nil
	}

	os.Remove(resolvConf)
	if err := os.Symlink(tsConf, resolvConf); err != nil {
		return nil
	}

	return nil
}

func (r *openbsdRouter) restoreResolvConf() error {
	if _, err := os.Stat(backupConf); err != nil {
		if os.IsNotExist(err) {
			return nil // No backup resolv.conf to restore.
		}
		return err
	}
	if ln, err := os.Readlink(resolvConf); err != nil {
		return err
	} else if ln != tsConf {
		return fmt.Errorf("resolv.conf is not a symlink to %s", tsConf)
	}
	if err := os.Rename(backupConf, resolvConf); err != nil {
		return err
	}
	os.Remove(tsConf) // Best effort removal.

	return nil
}