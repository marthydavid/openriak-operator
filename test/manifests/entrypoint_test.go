/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manifests

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	entrypointPath     = "../../images/riak/scripts/entrypoint.sh"
	nodeIdentityBegin  = "# --- node-identity: begin"
	nodeIdentityEnd    = "# --- node-identity: end ---"
	kubeletHostsHeader = "# Kubernetes-managed hosts file.\n" +
		"127.0.0.1\tlocalhost\n" +
		"::1\tlocalhost ip6-localhost ip6-loopback\n"
	podResolvConf = "search scale-test.svc.cluster.local svc.cluster.local cluster.local\n" +
		"nameserver 172.30.0.10\n" +
		"options ndots:5\n"
	podFQDN = "scale-c002-0.scale-c002-headless.scale-test.svc.cluster.local"
)

// nodeIdentityScript returns the entrypoint's node-naming block with /etc/hosts
// and /etc/resolv.conf pointed at test files, and the path of the hosts file.
func nodeIdentityScript(t *testing.T, hosts, resolv string) (script, hostsFile string) {
	t.Helper()
	raw, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	src := string(raw)
	begin := strings.Index(src, nodeIdentityBegin)
	end := strings.Index(src, nodeIdentityEnd)
	if begin < 0 || end < begin {
		t.Fatalf("entrypoint lost its node-identity markers")
	}
	block := src[begin:end]

	dir := t.TempDir()
	hostsFile = filepath.Join(dir, "hosts")
	resolvFile := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(hostsFile, []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolvFile, []byte(resolv), 0o600); err != nil {
		t.Fatal(err)
	}
	block = strings.ReplaceAll(block, "/etc/hosts", hostsFile)
	block = strings.ReplaceAll(block, "/etc/resolv.conf", resolvFile)
	return "set -eo pipefail\n" + block, hostsFile
}

var startingNode = regexp.MustCompile(`Starting Riak node: (\S+) \(name from (.*)\)`)

// runNodeIdentity runs the block with exactly env (plus PATH) and returns the
// chosen node name and its source, or the exit error and stderr. during, if not
// nil, runs while the script does.
func runNodeIdentity(t *testing.T, script string, env map[string]string, during func()) (
	node, source, stderr string, err error) {
	t.Helper()
	if _, lookErr := exec.LookPath("bash"); lookErr != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err = cmd.Start(); err != nil {
		t.Fatalf("start bash: %v", err)
	}
	if during != nil {
		during()
	}
	err = cmd.Wait()
	if m := startingNode.FindStringSubmatch(out.String()); m != nil {
		node, source = m[1], m[2]
	}
	return node, source, errOut.String(), err
}

// TestEntrypointNodeName guards issue #59: a node of an operator-managed cluster
// must always be named after the pod FQDN. A start under a short name persisted a
// ring that every later start (under the FQDN) crashed on.
func TestEntrypointNodeName(t *testing.T) {
	operatorEnv := func(podIP string) map[string]string {
		return map[string]string{
			"POD_NAME":          "scale-c002-0",
			"POD_IP":            podIP,
			"POD_NAMESPACE":     "scale-test",
			"RIAK_CLUSTER_NAME": "scale-c002",
			// The /etc/hosts retries are exercised by
			// TestEntrypointNodeNameRetriesEmptyHosts; here they only slow down
			// the fallback cases.
			"RIAK_HOSTS_RETRIES": "2",
		}
	}
	fullHosts := kubeletHostsHeader + "10.129.1.13\t" + podFQDN + "\tscale-c002-0\n"

	cases := []struct {
		name       string
		hosts      string
		resolv     string
		env        map[string]string
		wantNode   string
		wantSource string
		wantErr    string
	}{
		{
			name: "pod IP line in /etc/hosts", hosts: fullHosts, resolv: podResolvConf,
			env:      operatorEnv("10.129.1.13"),
			wantNode: "riak@" + podFQDN, wantSource: "pod IP 10.129.1.13",
		},
		{
			name: "POD_IP empty: found by pod name", hosts: fullHosts, resolv: podResolvConf,
			env:      operatorEnv(""),
			wantNode: "riak@" + podFQDN, wantSource: "hosts entry of scale-c002-0",
		},
		{
			name: "POD_IP not in /etc/hosts: found by pod name", hosts: fullHosts, resolv: podResolvConf,
			env:      operatorEnv("10.129.9.9"),
			wantNode: "riak@" + podFQDN, wantSource: "hosts entry of scale-c002-0",
		},
		{
			name:     "no FQDN in /etc/hosts: built from the headless Service",
			hosts:    kubeletHostsHeader + "10.129.1.13\tscale-c002-0\n",
			resolv:   podResolvConf,
			env:      operatorEnv("10.129.1.13"),
			wantNode: "riak@" + podFQDN, wantSource: "headless Service scale-c002-headless",
		},
		{
			name: "operator-managed and no FQDN anywhere: refuse to start", hosts: kubeletHostsHeader,
			resolv:  "nameserver 172.30.0.10\n",
			env:     operatorEnv(""),
			wantErr: "cannot determine this pod's FQDN",
		},
		{
			name: "standalone container keeps the short-name fallback", hosts: kubeletHostsHeader,
			resolv:   "nameserver 172.30.0.10\n",
			env:      map[string]string{"POD_NAME": "riak-0"},
			wantNode: "riak@riak-0", wantSource: "POD_NAME",
		},
		{
			name: "RIAK_NODE override wins", hosts: kubeletHostsHeader,
			resolv:   "nameserver 172.30.0.10\n",
			env:      map[string]string{"RIAK_CLUSTER_NAME": "c", "POD_NAME": "c-0", "RIAK_NODE": "riak@c-0.example.org"},
			wantNode: "riak@c-0.example.org", wantSource: "RIAK_NODE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script, _ := nodeIdentityScript(t, tc.hosts, tc.resolv)
			node, source, stderr, err := runNodeIdentity(t, script, tc.env, nil)
			if tc.wantErr != "" {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("want a failing exit, got err=%v node=%q", err, node)
				}
				if !strings.Contains(stderr, tc.wantErr) {
					t.Fatalf("stderr %q does not mention %q", stderr, tc.wantErr)
				}
				if node != "" {
					t.Fatalf("refused start still announced node %q", node)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected failure: %v\nstderr: %s", err, stderr)
			}
			if node != tc.wantNode {
				t.Fatalf("node = %q, want %q", node, tc.wantNode)
			}
			if !strings.Contains(source, tc.wantSource) {
				t.Fatalf("source = %q, want it to mention %q", source, tc.wantSource)
			}
		})
	}
}

// TestEntrypointNodeNameRetriesEmptyHosts reproduces the trigger of issue #59:
// the kubelet truncates and rewrites the pod's shared /etc/hosts whenever it
// creates another container of the pod (the metrics sidecar right after the Riak
// container), so the entrypoint can read it empty. The lookup must wait for the
// rewrite rather than give up on the FQDN.
func TestEntrypointNodeNameRetriesEmptyHosts(t *testing.T) {
	// No resolv.conf search domain: only a re-read of /etc/hosts can succeed.
	script, hostsFile := nodeIdentityScript(t, "", "nameserver 172.30.0.10\n")
	env := map[string]string{
		"POD_NAME":          "scale-c002-0",
		"POD_IP":            "10.129.1.13",
		"POD_NAMESPACE":     "scale-test",
		"RIAK_CLUSTER_NAME": "scale-c002",
	}
	node, source, stderr, err := runNodeIdentity(t, script, env, func() {
		time.Sleep(500 * time.Millisecond)
		content := kubeletHostsHeader + "10.129.1.13\t" + podFQDN + "\tscale-c002-0\n"
		if werr := os.WriteFile(hostsFile, []byte(content), 0o600); werr != nil {
			t.Error(werr)
		}
	})
	if err != nil {
		t.Fatalf("unexpected failure: %v\nstderr: %s", err, stderr)
	}
	if node != "riak@"+podFQDN {
		t.Fatalf("node = %q, want the FQDN once /etc/hosts is rewritten", node)
	}
	if !strings.Contains(source, "read ") {
		t.Errorf("source %q should say the hosts file was re-read", source)
	}
}
